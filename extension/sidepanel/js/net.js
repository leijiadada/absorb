// Absorb 阅读助手 - 网络层
// 职责：桥端口发现、连接重试、SSE 流解析、/api/ask 与 /api/cancel 通信。不碰 DOM。
import { t, errorByCode } from './i18n.js';

// 敏感日志开关：提交 payload 含用户问题与页面内容，发布版保持静默，排查时临时打开
const DEBUG = false;

// ---------- 桥端口发现 ----------
// 桌面端桥端口由 OS 动态分配：向 SW 请求当前端口（SW 经 Native 管道 get-port，
// 宿主实时读桌面端数据根端口文件），桌面端重启端口漂移后下次请求自动跟随。
// 端口未知（桌面端未启动/宿主未连接）返回 0，调用方按"桌面端未就绪"提示。
let _portInflight = null;

// 底层查询：{port, hostStatus}（hostStatus 为 SW 的宿主连接分类，见 background.js）
function queryBridge() {
  if (_portInflight) return _portInflight; // 并发调用共享同一次查询
  _portInflight = chrome.runtime.sendMessage({ type: 'get-bridge-port' })
    .then((resp) => ({
      port: resp && typeof resp.port === 'number' ? resp.port : 0,
      hostStatus: (resp && resp.hostStatus) || 'unknown'
    }))
    .catch(() => ({ port: 0, hostStatus: 'unknown' }))
    .finally(() => { _portInflight = null; });
  return _portInflight;
}

export function getBridgePort() {
  return queryBridge().then((r) => r.port);
}

// 宿主连接分类（installed/not-installed/unknown）："未安装引导卡片"判定源。
// unknown 时不定性（权限被禁等），宁漏报不误报。
export async function getHostStatus() {
  const { hostStatus } = await queryBridge();
  return hostStatus;
}

// 桥 base URL：端口未知时返回空串。
async function desktopBase() {
  const port = await getBridgePort();
  return port > 0 ? `http://localhost:${port}` : '';
}

// ---------- 网络重试（静默版） ----------
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// 仅连接类错误重试：TypeError=连接拒绝(服务未启动)、AbortError=连接超时。
// HTTP 状态码错误(4xx/5xx)请求已到达服务端,不在此层重试,由调用方分类处理。
// connectTimeoutMs 只覆盖到 fetch resolve(headers 到达),SSE 流阶段不设超时
// (思考模式+expand_context+LLM 推理正常链路可超 60s,误杀代价大于收益)。
export async function fetchWithRetry(url, opts = {},
    { attempts = 3, delays = [1000, 2000], connectTimeoutMs = 5000, externalSignal } = {}) {
  for (let i = 0; ; i++) {
    const ctrl = new AbortController();
    const timer = setTimeout(() => ctrl.abort(), connectTimeoutMs);
    // 外部 abort(用户主动停止)立即传播到本轮 fetch,不等连接超时
    const onExternalAbort = () => ctrl.abort();
    if (externalSignal) {
      if (externalSignal.aborted) ctrl.abort();
      else externalSignal.addEventListener('abort', onExternalAbort, { once: true });
    }
    try {
      return await fetch(url, { ...opts, signal: ctrl.signal });
    } catch (e) {
      // 外部主动 abort:不重试,直接抛(让上层区分"停止" vs "真错误")
      if (externalSignal && externalSignal.aborted) throw e;
      const retryable = (e instanceof TypeError) || e.name === 'AbortError';
      if (!retryable || i === attempts - 1) throw e;
      await sleep(delays[i] || delays[delays.length - 1]); // 指数退避 1s → 2s
    } finally {
      clearTimeout(timer);
      // 不在此 removeEventListener:fetch resolve 后 SSE body 仍在流式读取,
      // ctrl 必须保持对 externalSignal 的监听以把"停止"传播到 reader.read()。
      // listener 用 { once:true } 注册,abort 触发一次自动移除;未触发的随
      // ctrl 闭包在流式结束(_currentAbort=null)后一并 GC,无泄漏。
    }
  }
}

// 错误信息中文化(连接拒绝/超时/HTTP 错误/其他)
// 返回完整文案(不含 ⚠️ 前缀,由调用方统一加),技术细节下沉 console.error
export function classifyNetError(e) {
  if (e && e.name === 'AbortError') return t('errors.connectionTimeout');
  if (e instanceof TypeError) return t('errors.serviceNotRunning');
  // E3/E4/E5 的 Error.message 已是 i18n 文案,直接返回;未知错误兜底
  if (e && e.message) {
    console.error('[Absorb] 网络错误详情:', e);
    return e.message;
  }
  return t('errors.unknown');
}

// 主动停止：请求后端中断指定会话对应的 run（与连接断开关联）。
// 后端 run 已开启 DetachedCancel，不会随连接关闭被取消，必须显式调用该接口。
// 无需重试；fire-and-forget，失败/端口未知不影响本地"已停止"展示。
export function cancelRun(sessionId) {
  if (!sessionId) return;
  desktopBase().then((base) => {
    if (!base) return;
    fetch(base + '/api/cancel', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ sessionId })
    }).catch((e) => console.error('[Absorb] 取消请求失败:', e));
  });
}

// 拉取当前默认模型能力标志（/api/capabilities，视觉软闸数据源）。
// 返回 imageSupport 布尔值；桌面端未就绪/请求失败返回 null（调用方保持现状不降级）。
export async function fetchCapabilities() {
  try {
    const base = await desktopBase();
    if (!base) return null;
    const resp = await fetch(base + '/api/capabilities');
    if (!resp.ok) return null;
    const data = await resp.json();
    return typeof data.imageSupport === 'boolean' ? data.imageSupport : null;
  } catch (e) {
    console.error('[Absorb] 拉取模型能力失败:', e);
    return null;
  }
}

// 调用 Go 服务 /api/ask，解析 SSE 流，逐 token 回调
// 返回 { sessionId, broken }：
//   broken=true 表示 SSE 流中途断开(未收到 done 事件)，由调用方提供手动重试入口
// externalSignal:调用方传入的 AbortSignal,用户点"停止"时 abort,停止本地读取仅影响渲染
// ctxQuotes: 本次引用的划词数组 [{seq,text,paragraph,pageContext,url,pageMd}]（按输入顺序）
//   后端当前只认单条 context: context 取第一条(供页面缓存/参考),其余引用文本以编号拼进 question。
//   TODO: 后端支持多引用后,question 传占位 [[seq]] + 完整 quotes 数组。
// page: 当前浏览页 {url,title,pageMd} | null（无划词提问时后端注入 <page_content>/
// <page_url>：pageMd 非空注入页面内容（省 fetch_page），空则仅 URL 提示模型按需 fetch_page）
export async function callAgent(sendQ, ctxQuotes, sessionId, onToken, onReasoning, externalSignal, onSession, page, images = [], onPhase = null) {
  const quotes = Array.isArray(ctxQuotes) ? ctxQuotes : [];
  // 问题中标签占位 [[n]] 的替换形态：
  // 单条引用 → 材料开头短摘要作锚点，问题自然通顺，且模型不会模仿输出【引用n】编号泄漏给用户；
  // 多条引用 → 保留【引用n】编号，配合下方 material 段供模型对齐多条材料
  const digest = (t) => {
    const s = String(t || '').replace(/\s+/g, ' ').trim();
    return s.length > 12 ? s.slice(0, 12) + '…' : s;
  };
  let question;
  if (quotes.length === 1) {
    const anchor = digest(quotes[0].text) || '引用内容';
    question = (sendQ || '').replace(/\[\[(\d+)\]\]/g, () => anchor);
  } else {
    question = (sendQ || '').replace(/\[\[(\d+)\]\]/g, (_, n) => `【引用${n}】`);
  }
  // context：有划词取第一条（供页面缓存/参考）；
  // 无划词但采集到当前页内容（page.pageMd 非空）→ 构造纯页面 context，
  // Go 端同链路写缓存并注入 <page_content>，LLM 无需再调 fetch_page
  const context = quotes.length ? {
    text: quotes[0].text,
    paragraph: quotes[0].paragraph || '',
    pageContext: quotes[0].pageContext || '',
    url: quotes[0].url,
    pageMd: quotes[0].pageMd || ''
  } : (page && page.pageMd ? {
    text: '',
    paragraph: '',
    pageContext: '',
    url: page.url,
    pageMd: page.pageMd
  } : null);
  // 多条引用时把各标签全文以编号拼到问题前，供模型一次看到全部引用
  const material = quotes.length > 1
    ? quotes.map((q) => `【引用${q.seq}】\n${q.text}`).join('\n\n')
    : '';
  const finalQuestion = material ? material + '\n\n' + question : question;
  const payload = {
    question: finalQuestion,
    context,
    sessionId: sessionId || ''
  };
  // 当前浏览页（getActivePage 已过滤非 http(s)，采集失败为 null 静默降级）
  if (page && page.url) payload.page = page;
  // 图片附件：随请求发送（后端 v2 接入 images 字段；数据已在侧栏压缩为 dataURL）
  if (Array.isArray(images) && images.length) {
    payload.images = images.map((m) => ({ dataUrl: m.dataUrl, mime: m.mime, w: m.w, h: m.h }));
  }
  // 提交 payload 含用户问题与页面全文，仅 DEBUG 时打印（评估用）
  if (DEBUG) console.log('[Absorb] 提交 agent:', payload);
  // 先发现桥端口：未知（桌面端未启动/宿主未连接）时直接按未就绪提示，不进入重试；
  // 宿主注册缺失（未安装）单独文案，与引导卡片口径一致
  const probe = await queryBridge();
  if (!probe.port) {
    throw new Error(probe.hostStatus === 'not-installed'
      ? t('errors.desktopMissing')
      : t('errors.desktopNotReady'));
  }
  const base = `http://localhost:${probe.port}`;
  // 连接类错误(服务未启动/超时)自动重试 3 次;HTTP 状态码错误已到达服务端,不自动重试
  const resp = await fetchWithRetry(base + '/api/ask', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload)
  }, { externalSignal });
  if (!resp.ok) {
    console.error('[Absorb] HTTP 错误:', resp.status);
    throw new Error(resp.status >= 500
      ? t('errors.serverError')
      : t('errors.requestRejected'));
  }

  const reader = resp.body.getReader();
  const decoder = new TextDecoder();
  let buf = '';
  let gotSessionId = sessionId || '';
  let associations = null; // M3.1：done 前下发的关联卡片数据（无命中为 null）
  let askUser = null; // ask_user 挂起：模型提问卡片数据（ticketId/question/options）

  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    buf += decoder.decode(value, { stream: true });
    // SSE 事件以空行分隔
    let idx;
    while ((idx = buf.indexOf('\n\n')) >= 0) {
      const rawEvent = buf.slice(0, idx);
      buf = buf.slice(idx + 2);
      const dataLine = rawEvent.split('\n').find((l) => l.startsWith('data:'));
      if (!dataLine) continue;
      let evt;
      try { evt = JSON.parse(dataLine.slice(5).trim()); } catch { continue; }
      if (evt.type === 'reasoning') {
        onReasoning && onReasoning(evt.content || '');
      } else if (evt.type === 'token') {
        onToken(evt.content || '');
      } else if (evt.type === 'session') {
        gotSessionId = evt.sessionId || gotSessionId;
        onSession && onSession(gotSessionId);
      } else if (evt.type === 'phase') {
        // 中断恢复两阶段编排（resume.go）：恢复轮续答结束、用户问题轮开始。
        // 调用方据此切换到新消息气泡，恢复续答与问题回答分两条消息渲染。
        onPhase && onPhase();
      } else if (evt.type === 'ask_user') {
        // ask_user 挂起（官方 HITL）：模型提问卡片，done 带 pending:true
        askUser = {
          ticketId: evt.ticketId || '',
          question: evt.question || '',
          options: Array.isArray(evt.options) ? evt.options : []
        };
      } else if (evt.type === 'associations') {
        associations = {
          kind: evt.kind || '',
          candidates: Array.isArray(evt.candidates) ? evt.candidates : []
        };
      } else if (evt.done) {
        // requestId 随 done 下发（M1）：保存知识时作 originEventId 回传（与 sessionId 组幂等二元组）
        // pending:true 表示本轮以 ask_user 提问终止，续答经 /api/ask/answer
        return {
          sessionId: gotSessionId,
          requestId: evt.requestId || '',
          associations,
          pending: !!evt.pending,
          askUser,
          broken: false
        };
      } else if (evt.type === 'error') {
        console.error('[Absorb] 智能体错误事件:', evt);
        // 有稳定错误码 → 必走本地化码表；码未登记也用通用中文，绝不把英文 norm.Message 透给用户。
        // 无码（后端预检类错误，自带中文用户提示）→ 才回退 evt.message；二者皆缺则通用提示。
        let text;
        if (evt.code) {
          text = errorByCode(evt.code) || t('errors.agentError');
        } else {
          text = evt.message || t('errors.agentError');
        }
        const err = new Error(text);
        if (evt.code) err.code = evt.code; // 稳定码透传给上层做专项处理（如 1210 → 软闸回写）
        throw err;
      }
    }
  }
  // reader 结束但未收到 done 事件 → SSE 中途断流(服务崩溃/重启)
  return { sessionId: gotSessionId, broken: true };
}

// ask_user 续答（官方 HITL）：POST /api/ask/answer，用户回答由后端组装成
// 更新 JSON 发起续答 Run，SSE 流式返回（token/reasoning/done）。
// 返回 { requestId, broken }；错误语义与 callAgent 相同。
export async function callAgentAnswer(sessionId, ticketId, answer, onToken, onReasoning, externalSignal) {
  const probe = await queryBridge();
  if (!probe.port) {
    throw new Error(probe.hostStatus === 'not-installed'
      ? t('errors.desktopMissing')
      : t('errors.desktopNotReady'));
  }
  const base = `http://localhost:${probe.port}`;
  const resp = await fetchWithRetry(base + '/api/ask/answer', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ sessionId, ticketId, answer })
  }, { externalSignal });
  // 4xx 为校验类错误（JSON body），非 SSE：直接取 message 抛出
  if (resp.status >= 400 && resp.status < 500) {
    let msg = '';
    try { msg = (await resp.json())?.message || ''; } catch { /* 非 JSON body */ }
    throw new Error(msg || t('errors.requestRejected'));
  }
  if (!resp.ok) {
    throw new Error(t('errors.serverError'));
  }
  const reader = resp.body.getReader();
  const decoder = new TextDecoder();
  let buf = '';
  let requestId = '';
  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    buf += decoder.decode(value, { stream: true });
    let idx;
    while ((idx = buf.indexOf('\n\n')) >= 0) {
      const rawEvent = buf.slice(0, idx);
      buf = buf.slice(idx + 2);
      const dataLine = rawEvent.split('\n').find((l) => l.startsWith('data:'));
      if (!dataLine) continue;
      let evt;
      try { evt = JSON.parse(dataLine.slice(5).trim()); } catch { continue; }
      if (evt.type === 'reasoning') {
        onReasoning && onReasoning(evt.content || '');
      } else if (evt.type === 'token') {
        onToken(evt.content || '');
      } else if (evt.done) {
        requestId = evt.requestId || '';
        return { requestId, broken: false };
      } else if (evt.type === 'error') {
        console.error('[Absorb] 续答错误事件:', evt);
        const err = new Error(evt.code ? (errorByCode(evt.code) || t('errors.agentError')) : (evt.message || t('errors.agentError')));
        if (evt.code) err.code = evt.code;
        throw err;
      }
    }
  }
  return { requestId, broken: true };
}

// ---------- 知识保存（M2 吸收闭环） ----------

// 拉取保存面板预填草稿（POST /api/knowledge/draft）。
// 后端始终 200：ok=false 表示降级（模型不可用/材料不足/端点未配置），title 为
// 服务端兜底（问题截断）。网络层任何失败同样返回 {ok:false,title:'',summary:''}，
// 由面板本地兜底——草稿降级不是错误，用户仍可手写后保存。8s 超时防面板久等。
export async function fetchKnowledgeDraft({ question, answer, quote }) {
  try {
    const base = await desktopBase();
    if (!base) return { ok: false, title: '', summary: '' };
    const ctrl = new AbortController();
    const timer = setTimeout(() => ctrl.abort(), 8000);
    try {
      const resp = await fetch(base + '/api/knowledge/draft', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ question: question || '', answer: answer || '', quote: quote || '' }),
        signal: ctrl.signal
      });
      if (!resp.ok) return { ok: false, title: '', summary: '' };
      return await resp.json();
    } finally {
      clearTimeout(timer);
    }
  } catch (e) {
    console.error('[Absorb] 知识草稿拉取失败（降级为手写）:', e);
    return { ok: false, title: '', summary: '' };
  }
}

// 保存知识条目（POST /api/knowledge，幂等）。返回 {created, item}：
// created=true 新建（201），false 幂等命中（200，同 origin 二元组已保存过）。
// 走 fetchWithRetry（POST 幂等，连接类错误重试安全）；HTTP 错误抛出带 message
// 的 Error（服务端 message 已是中文用户文案），由面板行内展示。
export async function saveKnowledge(payload) {
  const base = await desktopBase();
  if (!base) throw new Error(t('errors.desktopNotReady'));
  const resp = await fetchWithRetry(base + '/api/knowledge', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload)
  });
  if (!resp.ok) {
    let msg = '';
    try { msg = (await resp.json())?.message || ''; } catch { /* body 非 JSON，用通用文案 */ }
    const err = new Error(msg || t('errors.knowledge.saveFailed'));
    err.status = resp.status;
    throw err;
  }
  return await resp.json();
}

// 文档准备（PDF 解读 PR3）：POST /api/page/prepare，服务端获取并提取文档正文
// 后落库，返回 {documentId,url,finalUrl,title,contentType,status,charCount,byteSize,truncated,message}。
// 分类状态（needs_auth/too_large 等）是 200 + status 产品语义，由调用方渲染；仅
// 请求体解析失败 400、内部错误 500。不加连接重试：这是最长 10 分钟的长操作
// （与服务端 prepareTimeout 对齐），失败重试由用户在文档卡上显式点"重试"。
export async function prepareDocument(url, title, externalSignal) {
  const base = await desktopBase();
  if (!base) throw new Error(t('errors.desktopNotReady'));
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), 10 * 60 * 1000);
  if (externalSignal) {
    if (externalSignal.aborted) ctrl.abort();
    else externalSignal.addEventListener('abort', () => ctrl.abort(), { once: true });
  }
  try {
    const resp = await fetch(base + '/api/page/prepare', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ url, title: title || '' }),
      signal: ctrl.signal
    });
    if (!resp.ok) {
      let msg = '';
      try { msg = (await resp.json())?.error || ''; } catch { /* body 非 JSON */ }
      throw new Error(msg || t('doccard.errorGeneric'));
    }
    return await resp.json();
  } finally {
    clearTimeout(timer);
  }
}

// 保存阅读摘要（POST /api/reading-summary，M2.5 层次A）：回答全文直接落到
// URL 文档锚点，幂等（同回答重复提交 created=false）。返回 {created, documentId}。
// 轻操作路径：不弹面板，调用方以按钮 morph + toast 反馈结果。
export async function saveReadingSummary(payload) {
  const base = await desktopBase();
  if (!base) throw new Error(t('errors.desktopNotReady'));
  const resp = await fetchWithRetry(base + '/api/reading-summary', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload)
  });
  if (!resp.ok) {
    let msg = '';
    try { msg = (await resp.json())?.message || ''; } catch { /* body 非 JSON，用通用文案 */ }
    const err = new Error(msg || t('knowledge.summarySaveFailed'));
    err.status = resp.status;
    throw err;
  }
  return await resp.json();
}
