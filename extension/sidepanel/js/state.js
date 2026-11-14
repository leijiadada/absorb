// Absorb 阅读助手 - 状态与存储层
// 职责：state/settings 单例、chrome.storage 持久化、划词消费、会话 helpers
import { STORAGE_KEY, SETTINGS_KEY, DRAFT_KEY } from './config.js';
import { fetchCapabilities } from './net.js';

// 敏感日志开关：消费日志含划词完整数据，发布版保持静默，排查时临时打开
const DEBUG = false;

// ---------- State ----------
export const state = {
  sessions: [],          // [{ id, sessionId, quotes, messages: [{role, content, ts, context}], createdAt, updatedAt }]
  currentSessionId: null
};
export const settings = {};

// 图片附件（内存态，不持久化）：截图/上传/粘贴的图片在提交时随请求发送，
// 不写入 chrome.storage（base64 会瞬间撑爆 5MB 配额）。提交后由 conversation.js 清空。
// 结构：[{ iid, dataUrl, mime, w, h }]
export const composerImages = [];

export function clearComposerImages() {
  composerImages.length = 0;
}

// 视觉模型可用性软闸：imageSupport 经 /api/capabilities 下发（见 refreshCapabilities），
// 持久化在 chrome.storage，每次打开面板启动时刷新；无缓存值时默认开放（宽容语义，
// 与后端 AskHandler 前置闸门 mc==nil 放行一致）
export function hasVisionModel() {
  return settings.imageSupport !== false;
}

// merge 写回 imageSupport 单字段：SETTINGS_KEY 下可能有其他来源的字段，不整体覆盖
async function persistImageSupport(value) {
  const data = await chrome.storage.local.get(SETTINGS_KEY);
  await chrome.storage.local.set({
    [SETTINGS_KEY]: { ...(data[SETTINGS_KEY] || {}), imageSupport: value }
  });
}

// 拉取后端默认模型能力并写入 settings 持久化（软闸数据源，经 /api/capabilities 下发）。
// 返回能力布尔值；null 表示未获取到（桌面端未就绪/请求失败），settings 保持现状不降级。
export async function refreshCapabilities() {
  const caps = await fetchCapabilities();
  if (caps === null) return null;
  settings.imageSupport = caps;
  try {
    await persistImageSupport(caps);
  } catch (e) {
    console.error('[Absorb] 能力标志持久化失败:', e);
  }
  return caps;
}

// 1210 纠偏回写（B2，只提示不重试）：模型实际拒收图片（元数据错标/配置未同步）时，
// 把软闸即时落为禁用并持久化，用户删图重发或切换视觉模型。注意边界：下次打开面板
// refreshCapabilities 以后端模型配置为准重新覆盖——若用户未在设置页修正元数据，
// 重开面板按钮可能恢复可用，届时再发图会再次触发本回写（与后端闸门兜底配合，安全）。
export function markImageUnsupported() {
  settings.imageSupport = false; // 同步先改内存，hasVisionModel() 立即生效
  persistImageSupport(false).catch((e) => console.error('[Absorb] 软闸回写持久化失败:', e));
}

// 流式互斥标记（ESM 导出原始值是快照，函数化保证各模块读写同一状态）
let _isStreaming = false;
export function getStreaming() { return _isStreaming; }
export function setStreaming(v) { _isStreaming = v; }

// ---------- Storage ----------
export async function loadState() {
  const data = await chrome.storage.local.get(STORAGE_KEY);
  // Object.assign 保持引用共享（不能重赋值，否则其他模块拿到的是旧对象）
  if (data[STORAGE_KEY]) Object.assign(state, data[STORAGE_KEY]);
  // 加载健壮性：保证每个会话 quotes 为数组（storage 数据异常时也不崩）
  for (const s of state.sessions) {
    if (!s.quotes) s.quotes = [];
  }
}

// 持久化快照：剥离 messages[].images[].full（内存态原图，仅供当轮点击放大）。
// 原图几百 KB/张，落 storage 会瞬间撑爆配额——只保留 192px 缩略，重开面板后
// 历史点击放大即为缩略（UI 提示已降采样）。无 full 时返回原引用（零拷贝）。
// 镜像上传（sync.js）复用同一剥离口径，保证落库与落 storage 体积一致。
export function stripFullImages(s) {
  if (!s.messages || !s.messages.some((m) => m.images && m.images.some((im) => im.full))) {
    return s;
  }
  return {
    ...s,
    messages: s.messages.map((m) => (m.images && m.images.some((im) => im.full))
      ? { ...m, images: m.images.map(({ full, ...rest }) => rest) }
      : m)
  };
}

function slimForStorage() {
  let dirty = false;
  const sessions = state.sessions.map((s) => {
    const slim = stripFullImages(s);
    if (slim !== s) dirty = true;
    return slim;
  });
  return dirty ? { ...state, sessions } : state;
}

export async function saveState() {
  try {
    await chrome.storage.local.set({ [STORAGE_KEY]: slimForStorage() });
  } catch (e) {
    // 配额溢出等持久化失败：剥离全部会话 quote 的 pageMd 后重试一次。
    // chip 展示只依赖 text，剥离不影响界面；调用方无需感知，本函数永不抛出
    console.error('[Absorb] 状态保存失败，剥离 pageMd 降级重试:', e);
    const slim = slimForStorage().sessions.map((s) => ({
      ...s,
      quotes: (s.quotes || []).map((q) => ({ ...q, pageMd: '' }))
    }));
    try {
      await chrome.storage.local.set({ [STORAGE_KEY]: { ...state, sessions: slim } });
    } catch (e2) {
      console.error('[Absorb] 状态保存仍失败（存储配额耗尽？），本次修改未持久化:', e2);
    }
  }
}

export async function loadSettings() {
  const data = await chrome.storage.local.get(SETTINGS_KEY);
  if (data[SETTINGS_KEY]) Object.assign(settings, data[SETTINGS_KEY]);
}

// 读取划词队列增量（游标法）：只推进已读 id 指针，不删除 pendingQuotes，
// 避免与 background append 竞争导致覆盖。返回待消费的升序划词数组，无新划词返回 null。
async function takePendingQuotes() {
  const data = await chrome.storage.local.get(['pendingQuotes', 'pendingCursor']);
  const list = Array.isArray(data.pendingQuotes) ? data.pendingQuotes : [];
  const cursor = Number(data.pendingCursor) || 0;

  const fresh = list
    .filter((q) => typeof q.id === 'number' && q.id > cursor)
    .sort((a, b) => a.id - b.id);
  if (fresh.length === 0) return null;

  const newCursor = fresh.reduce((m, q) => Math.max(m, q.id), cursor);
  await chrome.storage.local.set({ pendingCursor: newCursor });
  return fresh;
}

// 消费划词增量：追加入当前会话 quotes（多段划词 → 多个标签）。
// 返回本次新增的划词数组（供 UI 逐条追加标签），无新增返回 null；流式中返回 false。
// 串行化：init / onChanged / 流结束补消费三个入口可能并发调用，两次 takePendingQuotes
// 都读到未推进的游标会重复消费（重复 chip）。排到共享尾链上强制串行。
let _consumeTail = Promise.resolve();

export function consumePendingQuotes() {
  const run = async () => {
    if (getStreaming()) {
      console.log('[Absorb] consumePendingQuotes: 流式中，跳过');
      return false;
    }
    const fresh = await takePendingQuotes();
    if (DEBUG) console.log('[Absorb] consumePendingQuotes: fresh =', fresh);
    if (!fresh) return null;
    const s = currentSession() || newSession();
    s.quotes = [...(s.quotes || []), ...fresh];
    await saveState();
    return fresh;
  };
  const p = _consumeTail.then(run, run);
  _consumeTail = p.then(() => {}, () => {}); // 失败不阻塞后续消费
  return p;
}

// ---------- Session helpers ----------
// uid 本地会话键（恢复注入也用它生成本地 id，远端归并以 sessionId 为准）
export function uid() { return Date.now().toString(36) + Math.random().toString(36).slice(2, 6); }

export function currentSession() {
  return state.sessions.find((s) => s.id === state.currentSessionId) || null;
}

export function newSession() {
  const s = {
    id: uid(),
    sessionId: '',        // Go 侧会话 ID（首次回答后回填，用于多轮上下文）
    quotes: [],           // 划词标签集合：[{ id, text, paragraph, pageContext, url, pageMd }]
    messages: [],
    createdAt: Date.now(),
    updatedAt: Date.now()
  };
  state.sessions.unshift(s);
  state.currentSessionId = s.id;
  // 仅保留最近 50 条会话，避免 storage 膨胀
  state.sessions = state.sessions.slice(0, 50);
  return s;
}

export function getOrCreateCurrent() {
  return currentSession() || newSession();
}

// ---------- 输入框草稿 ----------
// 独立 storage 键（不并入 state 整包序列化），防抖 300ms 写入，关面板不丢。
// 存的是 serializeComposer() 的 JSON 快照：{ text（含 [[n]] 占位）, refs（chip qid 顺序表） }
let _draftTimer = null;

export function saveDraftSoon(serialized) {
  clearTimeout(_draftTimer);
  _draftTimer = setTimeout(() => {
    chrome.storage.local.set({ [DRAFT_KEY]: serialized }).catch(() => {});
  }, 300);
}

export async function loadDraft() {
  const data = await chrome.storage.local.get(DRAFT_KEY);
  const raw = data[DRAFT_KEY];
  if (typeof raw !== 'string' || !raw) return null;
  try {
    const obj = JSON.parse(raw);
    if (obj && typeof obj.text === 'string' && Array.isArray(obj.refs)) return obj;
  } catch { /* 脏数据按无草稿处理 */ }
  return null;
}

export function clearDraft() {
  clearTimeout(_draftTimer);
  return chrome.storage.local.remove(DRAFT_KEY).catch(() => {});
}
