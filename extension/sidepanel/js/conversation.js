// Absorb 阅读助手 - 对话流程层
// 职责：提问提交、流式回答编排（思考块/token/断流重试）、划词积压处理、轮数软提示
import {
  state, getStreaming, setStreaming, saveState,
  currentSession, newSession, consumePendingQuotes, composerImages, clearComposerImages,
  hasVisionModel, markImageUnsupported
} from './state.js';
import { callAgent, callAgentAnswer, cancelRun, classifyNetError } from './net.js';
import { uploadMirror } from './sync.js';
import { keepContextFrom, attachKnowledgeEntry, attachAssociations } from './knowledge.js';
import { t } from './i18n.js';
import {
  els, buildMsgNode, renderMarkdown, renderAssistantContent, decorateMermaid, renderComposerChips, appendNewQuoteChip, chipText, updatePlaceholder,
  renderMessages, renderHistoryList, serializeComposer, syncDraft,
  scrollToBottom, isNearBottom, updateAskBtn, showToast, hideToast
} from './render.js';
import { renderAttachments, setImageGate } from './image.js';
import { isPdfUrl, readyDocumentId } from './doccard.js';

// 当前流式的 AbortController(模块级,stopStreaming 与 streamAssistantReply 共享)
// null 表示无流式在跑;主动停止时 abort,后端框架 ctx 取消自动停止生成
let _currentAbort = null;

// 流式渲染节流间隔：每 token 全量重渲 markdown 是 O(n²)，合帧后渲染频率与 token 速率解耦
const STREAM_RENDER_INTERVAL_MS = 80;

// ---------- 划词消费编排（数据 + UI 互斥单元）----------
// init / onChanged / 流结束补消费统一走这里：state 层数据消费与 chip 落框（UI）
// 在同一个互斥单元内完成。submitQuestion 开头等待 _consumeInFlight 清空，
// 保证 extract 读到的输入框 chip 与会话 quotes 一致——根治"流结束瞬间提交"竞态
// （消费链 saveState 的 IPC 窗口内提交 → s.quotes=[] 抹掉刚写入的引用 → chip 迟到
// 落框成"幽灵" → 下一轮提问裸 [[1]] 且无参考材料）。
let _consumeInFlight = null;

export function consumeAndRenderChips() {
  const run = async () => {
    const fresh = await consumePendingQuotes();
    if (fresh && fresh.length) {
      fresh.forEach((q) => appendNewQuoteChip(q));
      renderMessages();
      renderHistoryList();
      updatePlaceholder();
    }
    return fresh;
  };
  const prev = _consumeInFlight || Promise.resolve();
  const p = prev.then(run, run); // 已有消费在跑：排队其后，不丢本次触发
  _consumeInFlight = p;
  p.finally(() => { if (_consumeInFlight === p) _consumeInFlight = null; });
  return p;
}

// ---------- Ask flow ----------
// 采集当前活动 tab 的 url/title + 整页结构化文本（pageMd）：无划词提问时
// 让 agent 知道用户在看哪个页面并直接获得页面内容（省掉 LLM 的 fetch_page 轮次，
// 且对 SPA/内网页面——HTTP 抓取拿不到渲染态内容——是唯一有效通道）。
// 提交时实时 query（非面板打开时缓存），保证"问的是正在看的页"；
// 无 tabs 权限，url/title 仅在 activeTab 授权期内可见（图标/右键菜单调用且未跨源导航），
// 授权失效时 tab.url 为 undefined → 返回 null，宁缺勿错；
// 只发 http(s)——chrome://、file:// 抓不到，静默跳过；PDF 标签页走文档卡
// documentId 通道（见下方 PDF 分支）；
// 页面采集与划词共用 grab.js 链路（无划词走 grabPageOnly 形态），失败静默降级为仅 url/title
// （后端回退 fetch_page 现状行为）。
async function getActivePage() {
  try {
    const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
    if (!tab || !tab.url || !/^https?:\/\//i.test(tab.url)) return null;
    // PDF 标签页：内置查看器非普通 DOM 文档，grab.js 注入不可行；正文经文档卡
    // /api/page/prepare 提取落库，已就绪时透传 documentId，后端注入已提取正文
    // （未读取/失败时不带 documentId，维持原降级行为）
    if (isPdfUrl(tab.url)) {
      return {
        url: tab.url, title: tab.title || '', pageMd: '',
        documentId: await readyDocumentId(tab.url)
      };
    }
    const page = { url: tab.url, title: tab.title || '', pageMd: '' };
    try {
      const results = await chrome.scripting.executeScript({
        target: { tabId: tab.id },
        files: ['sidepanel/vendor/defuddle.min.js', 'html-pruner.js', 'pageconvert.js', 'grab.js']
      });
      const r = results && results[0] && results[0].result;
      if (r && r.pageMd) page.pageMd = r.pageMd;
    } catch (e) {
      console.warn('[Absorb] 无划词页面采集失败，回退 fetch_page:', e);
    }
    return page;
  } catch {
    return null;
  }
}

export async function submitQuestion() {
  console.log('[Absorb] submitQuestion 触发, isStreaming =', getStreaming());
  if (getStreaming()) return;
  // 提交互斥：等待进行中的划词消费链（含 chip 落框）结束再 extract。
  // while 防等待期间又有新消费入链；等待通常仅为一次 storage IPC（几十 ms）
  while (_consumeInFlight) {
    await _consumeInFlight;
  }
  const { text, refs } = serializeComposer();
  // 图片附件（内存态，不进输入框序列化）：本轮回传，提交后清空
  const images = composerImages.slice();
  // 无任何内容（无文本、无标签、无图片）→ 忽略。
  // 注意：不可因 text 为空就直接 return——只附图片（未输入文字）也必须能提交
  const bodyText = String(text || '').replace(/\[\[\d+\]\]/g, '').trim();
  if (!bodyText && refs.length === 0 && images.length === 0) {
    console.log('[Absorb] submitQuestion: 内容为空，跳过');
    return;
  }
  // 仅附图片、未输入文字时，补一条默认指令，避免把空问题发给模型
  const DEFAULT_IMAGE_PROMPT = '请解读图片内容';
  const questionText = (!bodyText && refs.length === 0 && images.length > 0) ? DEFAULT_IMAGE_PROMPT : text;
  // 由 session.quotes 按标签输入顺序选出本次引用的完整数据（附 seq）
  const sCur = currentSession() || newSession();
  const byId = new Map((sCur.quotes || []).map((q) => [String(q.id), q]));
  const refQuotes = [];
  for (let i = 0; i < refs.length; i++) {
    const q = byId.get(String(refs[i]));
    if (q) refQuotes.push({ seq: i + 1, ...q });
  }
  // 发送用：保留内部占位符 [[seq]]，供后端关联编号
  const sendQ = questionText;
  // 展示用：同样保留占位符——气泡渲染时由 buildMsgNode 按消息快照把 [[n]] 行内还原为只读标签
  // （与提问框视觉一致）。此处若先替换成文字，渲染层就再也拿不到标签位置了
  const displayQ = questionText;
  els.questionInput.textContent = '';
  syncDraft(); // 提交后输入框已清，草稿同步清空
  updateAskBtn();
  // 清空图片附件（已拷贝进 images 随请求发送，不持久化）
  clearComposerImages();
  renderAttachments();
  updateAskBtn();
  // 追问复用当前会话（保留 sessionId + 历史）；无当前会话才新建
  let s = currentSession();
  if (!s) s = newSession();
  // 提交时实时采集当前浏览页，随请求发给 agent（无划词提问时后端注入 <page_url>）
  const page = await getActivePage();
  await runConversationOn(s, displayQ, sendQ, refQuotes, page, images);
}

// 把问题推进会话并触发流式回答
// 微信引用语义:提交时快照引用的多个标签到该条 user 消息,随后清空 s.quotes;
// 追问上下文由 Go 端 RecentReferenceMaterial 从 SQLite 兜底重申,不依赖前端 context
async function runConversationOn(s, displayQ, sendQ, refQuotes, page, images) {
  // 摘要快照(仅 text + url,剥离 pageMd 防 storage 膨胀),供历史渲染引用前缀与
  // M2 知识保存组装来源(url 是证据的一部分:保存时 sourceUrl 存真实页面地址)
  const ctxDisplay = refQuotes.map((q) => ({ text: q.text, url: q.url || '' }));
  // 图片存轻量尺寸 + 192px 缩略（持久化）+ 内存态原图 full（方案 D：当轮点击放大
  // 用原图，saveState 序列化时剥离 full 不落 storage，重开面板后仅剩缩略）
  const msgImages = (Array.isArray(images) && images.length)
    ? images.map((m) => ({ w: m.w, h: m.h, thumb: m.thumb, full: m.dataUrl }))
    : undefined;
  s.messages.push({
    role: 'user', content: displayQ, ts: Date.now(),
    context: {
      quotes: ctxDisplay,
      // 页面定位(M2):知识保存的来源回链用;只存 url+title,pageMd 不落 storage
      page: page && page.url ? { url: page.url, title: page.title || '' } : undefined
    },
    images: msgImages
  });
  s.updatedAt = Date.now();
  s.quotes = [];
  renderComposerChips();
  renderMessages();
  await saveState();
  await streamAssistantReply(s, displayQ, sendQ, refQuotes, page, images);
  s.updatedAt = Date.now();
  await saveState();
  // 桌面端镜像上传（成功/停止/断流各路径都收敛到这里）：全量幂等覆盖，失败静默自愈
  uploadMirror(s);
}

// 调用 Go 服务，把流式 token 追加渲染到消息气泡。
// 中断恢复（后端两阶段编排）：一次 SSE 连接内可能收到 {type:"phase"} 边界事件
// （恢复轮续答结束、用户问题轮开始）→ switchPhase 定格当前气泡并开启新气泡，
// 恢复续答与问题回答分两条消息独立渲染、独立挂知识入口。
export async function streamAssistantReply(s, question, sendQ, ctxQuotes, page, images) {
  // M2:流成功后用末条 user 消息(本轮回话的提问快照)组装知识保存上下文
  const userMsg = s.messages[s.messages.length - 1];
  els.emptyTip.style.display = 'none';

  _currentAbort = new AbortController();
  setStreaming(true);
  updateAskBtn();

  // 当前流式气泡的可变状态（phase 事件切换时整体重建）
  const createBubble = () => {
    // reasoning 持久化到消息对象,刷新后由 buildMsgNode 重建折叠块(默认折叠,与流式结束时一致)
    const msg = { role: 'assistant', content: '', reasoning: '', ts: Date.now() };
    s.messages.push(msg);
    const node = buildMsgNode('assistant', '', true);
    els.messages.appendChild(node);
    const bubble = node.querySelector('.bubble');
    const caret = bubble.querySelector('.stream-caret');
    // 思考过程折叠块:流式时展开让用户看见思考;首个正式 token 到达时折叠(防抢焦点、防闪烁)
    const reasoningEl = document.createElement('details');
    reasoningEl.className = 'reasoning';
    reasoningEl.open = true;
    const reasoningSummary = document.createElement('summary');
    reasoningSummary.textContent = t('process.thinking');
    const reasoningBody = document.createElement('div');
    reasoningBody.className = 'reasoning-body';
    reasoningEl.appendChild(reasoningSummary);
    reasoningEl.appendChild(reasoningBody);
    node.insertBefore(reasoningEl, bubble);
    return { msg, node, bubble, caret, reasoningEl, reasoningSummary, reasoningBody, reasoningDone: false };
  };
  let cur = createBubble();
  scrollToBottom();

  // 节流渲染：token 只累积进 content，80ms 合帧刷一次 DOM。流式帧跳过 hljs 高亮
  // （opts.highlight=false，最重开销），流结束终帧 renderStream(true) 补上。
  // 三条结束路径（成功/停止/错误）都清掉挂起帧或各自渲染，旧内容帧不会回写覆盖
  let renderTimer = null;
  const renderStream = (final = false) => {
    clearTimeout(renderTimer);
    renderTimer = null;
    // 助手正文统一走 renderAssistantContent：可视化块流式期显示占位，终帧解析渲染
    cur.bubble.textContent = '';
    cur.bubble.appendChild(renderAssistantContent(cur.msg.content, s.sessionId, { highlight: final, final }));
    if (cur.caret) cur.bubble.appendChild(cur.caret);
    if (isNearBottom()) scrollToBottom();
  };
  const scheduleStreamRender = () => {
    if (renderTimer) return;
    renderTimer = setTimeout(() => renderStream(), STREAM_RENDER_INTERVAL_MS);
  };
  // phase 边界：定格当前气泡（终帧渲染、去光标/streaming 态）并开启新气泡
  const switchPhase = () => {
    renderStream(true);
    cur.node.classList.remove('streaming');
    if (cur.caret) cur.caret.remove();
    cur = createBubble();
    if (isNearBottom()) scrollToBottom();
  };
  // 智能粘底:每个 token 渲染时实时判断滚动位置——
  // 输出中途上滑超 80px 即停止跟随,滚回底部自动恢复;强制 reflow 每 token 本就发生(读 scrollHeight),无新增开销

  let streamResult = null;
  try {
    streamResult = await callAgent(sendQ, ctxQuotes, s.sessionId, (token) => {
      // 首个正式 token → 思考结束:有思考则折叠显示字数,无思考则移除空块
      if (!cur.reasoningDone) {
        cur.reasoningDone = true;
        if (cur.reasoningBody.textContent.length === 0) {
          cur.reasoningEl.remove();
        } else {
          cur.reasoningEl.open = false;
          cur.reasoningSummary.textContent = t('process.thinkingDone');
        }
      }
      cur.msg.content += token;
      scheduleStreamRender();
    }, (reasoningChunk) => {
      // 思考过程流式追加(GLM-4.5+ 思考模式,reasoning 与 token 互斥,不会重复)
      // 同步累积到 msg.reasoning,刷新后可由 buildMsgNode 重建折叠块
      cur.msg.reasoning += reasoningChunk;
      cur.reasoningBody.textContent += reasoningChunk;
      if (isNearBottom()) scrollToBottom();
    }, _currentAbort.signal, (sid) => {
      // 实时拿到服务端 sessionId,供停止时定位 /api/cancel 对应的 run
      if (sid) s.sessionId = sid;
    }, page, images, () => {
      // 中断恢复编排（resume.go）：恢复轮续答结束，开启新气泡渲染用户问题轮
      switchPhase();
    });
  } catch (err) {
    if (_currentAbort && _currentAbort.signal.aborted) {
      // 主动停止:保留半截 content+reasoning,气泡尾部追加"已停止"标记(与刷新后 buildMsgNode 一致)
      cur.msg.stopped = true;
      renderStream(true); // 终帧:半截内容(带高亮)完整上屏,"已停止"标记才有正确落点
      const mark = document.createElement('span');
      mark.className = 'stop-mark';
      mark.textContent = t('buttons.stopped');
      cur.bubble.appendChild(mark);
    } else {
      // 连接拒绝/超时(已重试 3 次)、HTTP 错误 → 统一中文提示
      cur.msg.content = '⚠️ ' + classifyNetError(err);
      cur.bubble.innerHTML = renderMarkdown(cur.msg.content, s.sessionId);
      // 服务未连上谈不上思考：移除空的"思考中"占位，避免误导
      if (cur.reasoningBody.textContent.length === 0) cur.reasoningEl.remove();
      // 1210 纠偏（B2，只提示不重试）：模型实际拒收图片（如元数据错标的 GLM）→
      // 错误文案已随气泡展示，此处回写软闸即时禁用图片入口；重开面板按后端
      // capabilities 重新校准（见 state.markImageUnsupported 的边界说明）
      if (err && err.code === 'model.image_unsupported') {
        markImageUnsupported();
        setImageGate(hasVisionModel());
      }
      // 连接类失败(服务未启动/超时)→ 显示重连条（固定于输入区上方，不随消息滚动）
      if (err instanceof TypeError || (err && err.name === 'AbortError')) {
        els.reconnectBar.classList.add('error');
        els.reconnectBar.hidden = false;
        els.reconnectMsg.textContent = t('errors.testConnFailed');
      }
      // 错误路径全程无 token、无滚动（L140 不执行），补一次滚底让错误气泡进入视野
      scrollToBottom();
    }
  }
  // 清掉挂起的节流帧:错误路径已直渲错误文案,旧内容帧不得回写覆盖
  clearTimeout(renderTimer);
  renderTimer = null;
  // 终帧:流结束时代码块语法已完整,补一次带高亮的最终渲染(停止/错误路径已在 catch 内渲染)
  // 终帧前采样跟随状态:随后的渲染/附加条目都会长高,事后再测 isNearBottom 恒为 false;
  // 只有全程跟随流式输出的用户才需要补滚底(中途上滑阅读的不劫持滚动位置)
  const follow = isNearBottom();
  if (streamResult) {
    renderStream(true);
    s.sessionId = streamResult.sessionId || s.sessionId;
    if (streamResult.pending && streamResult.askUser) {
      // ask_user 挂起（官方 HITL）：本轮以模型提问终止。气泡定格提问文本，
      // 渲染回答卡片；回答经 /api/ask/answer 续答进新气泡，知识入口挂续答轮。
      // 提问轮无 token，空的"思考中"占位不会经 token 回调清理，此处手动移除
      if (cur.reasoningBody.textContent.length === 0) cur.reasoningEl.remove();
      cur.msg.content = streamResult.askUser.question;
      renderStream(true);
      cur.node.classList.remove('streaming');
      if (cur.caret) cur.caret.remove();
      const answer = await waitAskAnswer(cur, streamResult.askUser);
      if (answer === null) {
        // 卡片等待期主动停止：移除卡片，气泡标记已停止（不续答）
        cur.msg.stopped = true;
        const mark = document.createElement('span');
        mark.className = 'stop-mark';
        mark.textContent = t('buttons.stopped');
        cur.bubble.appendChild(mark);
      } else {
        cur = createBubble();
        let contResult = null;
        try {
          contResult = await callAgentAnswer(s.sessionId, streamResult.askUser.ticketId, answer,
            (token) => {
              if (!cur.reasoningDone) {
                cur.reasoningDone = true;
                if (cur.reasoningBody.textContent.length === 0) cur.reasoningEl.remove();
                else { cur.reasoningEl.open = false; cur.reasoningSummary.textContent = t('process.thinkingDone'); }
              }
              cur.msg.content += token;
              scheduleStreamRender();
            },
            (rc) => {
              cur.msg.reasoning += rc;
              cur.reasoningBody.textContent += rc;
              if (isNearBottom()) scrollToBottom();
            },
            _currentAbort ? _currentAbort.signal : undefined);
          renderStream(true);
          if (contResult.requestId && userMsg && userMsg.role === 'user') {
            cur.msg.requestId = contResult.requestId;
            attachKnowledgeEntry(cur.node, keepContextFrom(s, userMsg, cur.msg));
          }
        } catch (err) {
          if (_currentAbort && _currentAbort.signal.aborted) {
            cur.msg.stopped = true;
            renderStream(true);
            const mark = document.createElement('span');
            mark.className = 'stop-mark';
            mark.textContent = t('buttons.stopped');
            cur.bubble.appendChild(mark);
          } else {
            cur.msg.content = '⚠️ ' + classifyNetError(err);
            cur.bubble.innerHTML = renderMarkdown(cur.msg.content, s.sessionId);
          }
        }
        decorateMermaid(cur.bubble);
      }
    } else if (streamResult.requestId && userMsg && userMsg.role === 'user') {
      // M2:done 事件携带 requestId(幂等锚点)→ 写入消息快照并挂"留下来"入口;
      // 断流(broken)无 requestId 天然不挂,重试成功后由新一轮补挂。
      // 两阶段恢复：done 是最后一轮(用户问题轮)的,挂在其气泡上——知识上下文的
      // userMsg 即用户真实问题,恢复续答只是前置铺垫。
      cur.msg.requestId = streamResult.requestId;
      attachKnowledgeEntry(cur.node, keepContextFrom(s, userMsg, cur.msg));
      // M3.1：低干扰关联卡片（后端有命中才下发事件；无事件 = 无可靠关联，不渲染）
      if (streamResult.associations && streamResult.associations.candidates.length) {
        attachAssociations(cur.node, streamResult.associations);
      }
    }
  }
  setStreaming(false);
  _currentAbort = null;
  updateAskBtn();
  cur.node.classList.remove('streaming');
  const c = cur.node.querySelector('.stream-caret');
  if (c) c.remove();
  // 流式结束：mermaid 代码块此时语法已完整，替换为 SVG 图（失败降级保持代码块）
  decorateMermaid(cur.bubble);
  // 终帧长高后补滚底:关联卡片/知识留存条/mermaid SVG 都插在最后一次 token 滚动之后,
  // 不补滚会停在旧位置,关联卡片被滚动边界切掉一半(用户实测反馈)
  if (follow) scrollToBottom();

  // 断流恢复:流中断开(未收到 done)→ 半截回答尾部提供手动重试。
  // 不自动重发(防消息重复/重复计费);重试移除半截 assistant 后原样重发该问题
  if (streamResult && streamResult.broken) {
    const bar = document.createElement('div');
    bar.className = 'retry-bar';
    const label = document.createElement('span');
    label.textContent = t('errors.streamBroken');
    const btn = document.createElement('button');
    btn.textContent = t('buttons.retry');
    btn.addEventListener('click', () => {
      if (getStreaming()) return;
      btn.disabled = true; // 防连点重复发送
      // 移除半截 assistant 消息(数据 + DOM),user 消息保留
      // （两阶段恢复下断流只会断在最后一轮，cur 即末条 assistant）
      const i = s.messages.indexOf(cur.msg);
      if (i >= 0) s.messages.splice(i, 1);
      cur.node.remove();
      // 原样重发:不重复 push user 消息(UI 已有),Go 端会追加新一轮问答
      // （question 修正：displayQ 在本作用域不存在，原写法重试会抛 ReferenceError）
      streamAssistantReply(s, question, sendQ, ctxQuotes, page).then(async () => {
        await saveState();
        uploadMirror(s); // 重试路径不经 runConversationOn 收尾，镜像同步在此补挂
      });
    });
    bar.appendChild(label);
    bar.appendChild(btn);
    cur.bubble.appendChild(bar);
  }
  // 流式结束后处理积压的划词（面板已打开时再次右键）：新标签逐条追加进输入框
  setTimeout(() => {
    if (getStreaming()) return;
    consumeAndRenderChips();
  }, 50);
  // 软提示：达到 50 轮后非阻塞提醒用户新建会话
  maybeWarnRounds(s);
}

// 用户主动停止:
// 1) abort 本地 fetch,停止继续读取/渲染,气泡标记"已停止";
// 2) 调 /api/cancel 显式中断后端 run —— 后端已 DetachedCancel,不会随连接关闭被取消,
//    必须显式取消,否则任务会继续跑完。
// 不在 stopStreaming 改 UI,停止后的 UI 由 streamAssistantReply 的 catch 分支统一处理。
export function stopStreaming() {
  if (!getStreaming() || !_currentAbort) return;
  _currentAbort.abort();
  const s = currentSession();
  cancelRun(s && s.sessionId);
}

// ---------- ask_user 回答卡片 ----------
// 在提问气泡内渲染交互卡片（选项胶囊 + 自由文本），提交后 resolve 回答文本。
// 提问正文由调用方写入气泡（cur.msg.content），卡片只承载可交互区——不重复渲染
// 问题，刷新后气泡文本持久、卡片消失，语义自洽。
// 卡片等待期监听 _currentAbort：用户点"停止"→ resolve(null)（不续答，气泡标记已停止）。
function waitAskAnswer(cur, askUser) {
  return new Promise((resolve) => {
    const card = document.createElement('div');
    card.className = 'ask-card';

    let settled = false;
    const submit = (val) => {
      const v = String(val || '').trim();
      if (!v || settled) return;
      settled = true;
      if (_currentAbort) _currentAbort.signal.removeEventListener('abort', onAbort);
      card.remove();
      resolve(v);
    };
    const onAbort = () => {
      if (settled) return;
      settled = true;
      card.remove();
      resolve(null);
    };
    if (_currentAbort) _currentAbort.signal.addEventListener('abort', onAbort);

    (askUser.options || []).forEach((opt) => {
      const pill = document.createElement('button');
      pill.className = 'ask-card-opt';
      pill.textContent = opt;
      pill.addEventListener('click', () => submit(opt));
      card.appendChild(pill);
    });

    const row = document.createElement('div');
    row.className = 'ask-card-row';
    const input = document.createElement('input');
    input.className = 'ask-card-input';
    input.placeholder = t('ask.placeholder');
    input.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') submit(input.value);
    });
    const send = document.createElement('button');
    send.className = 'ask-card-send';
    send.textContent = t('ask.send');
    send.addEventListener('click', () => submit(input.value));
    row.appendChild(input);
    row.appendChild(send);
    card.appendChild(row);

    cur.bubble.appendChild(card);
    if (isNearBottom()) scrollToBottom();
    input.focus();
  });
}

// ---------- 会话轮数软提示 ----------
// 一轮 = user + assistant 一对，约 messages.length / 2
// 50 轮软提示：非阻塞 toast，3 秒消失，不打断阅读节奏
const SESSION_WARN_ROUNDS = 50;

function getRounds(s) {
  if (!s || !s.messages) return 0;
  return Math.floor(s.messages.length / 2);
}

function maybeWarnRounds(s) {
  const n = getRounds(s);
  if (n < SESSION_WARN_ROUNDS) return;
  showToast({
    text: t('soft.roundsWarn', { rounds: n }),
    variant: 'info',
    persist: false,
    actions: [
      { label: t('buttons.newSession'), primary: true, onClick: () => {
        state.currentSessionId = null;
        renderHistoryList();
        renderComposerChips();
        renderMessages();
        hideToast();
      }},
      { label: t('buttons.continue'), onClick: () => hideToast() }
    ]
  });
}
