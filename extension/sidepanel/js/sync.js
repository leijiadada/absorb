// Absorb 阅读助手 - 会话镜像同步层（design/ext-session-sync-design.md）
// 职责：整会话对象镜像上传（PUT mirror）、桌面端索引拉取与 diff 回灌、远端会话懒加载。
// 桌面端 sessions.db 为权威源，chrome.storage 为工作集缓存；桌面不可达时按本地模式工作。
// 上传为全量幂等覆盖，失败静默——下一轮问答自动重传自愈，无队列无重试。不碰 DOM。
import { state, saveState, uid, stripFullImages } from './state.js';
import { getBridgePort } from './net.js';

// 敏感日志开关：镜像含完整会话正文，发布版保持静默，排查时临时打开
const DEBUG = false;

// 索引缓存有效期：面板打开时拉取（全量拉取为定稿决策），短 TTL 仅防抽屉
// 反复开关时重复请求；跨过 TTL 的下次打开抽屉重新拉取
const INDEX_TTL_MS = 30_000;

// 详情缓存上限（远端注入的会话计入 sessions 总量，超出驱逐最旧的远端缓存）
const SESSION_CACHE_LIMIT = 50;

// 远端索引 stubs：[{ sessionId, title, msgCount, createdAt, updatedAt }]（内存态，
// 每次面板打开按决策重新全量拉取，不落 storage）
let _remoteIndex = null;
let _remoteIndexAt = 0;

// ---------- 镜像上传 ----------

// uploadMirror 整会话对象镜像上传。仅在拿到服务端 sessionId 后才有归属键；
// 端口未知/失败静默（fire-and-forget），调用方不感知结果。
export function uploadMirror(s) {
  if (!s || !s.sessionId) return;
  (async () => {
    try {
      const port = await getBridgePort();
      if (!port) return;
      const resp = await fetch(
        `http://localhost:${port}/api/ext/sessions/${encodeURIComponent(s.sessionId)}/mirror`,
        {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          // 与 saveState 同剥离口径：内存态原图 full 不出端点
          body: JSON.stringify(stripFullImages(s))
        });
      if (!resp.ok) console.warn('[Absorb] 会话镜像上传失败: HTTP', resp.status);
      else if (DEBUG) console.log('[Absorb] 会话镜像已上传:', s.sessionId);
    } catch (e) {
      console.warn('[Absorb] 会话镜像上传失败（下一轮问答自动重传）:', e);
    }
  })();
}

// ---------- 索引拉取与回灌 ----------

// refreshRemoteIndex 全量拉取桌面端索引（force=true 手动入口跳过 TTL）。
// 返回索引数组；桌面不可达/失败返回 null（调用方保持本地模式）。
export async function refreshRemoteIndex(force = false) {
  if (!force && _remoteIndex && Date.now() - _remoteIndexAt < INDEX_TTL_MS) {
    return _remoteIndex;
  }
  try {
    const port = await getBridgePort();
    if (!port) {
      _remoteIndex = null;
      return null;
    }
    const resp = await fetch(`http://localhost:${port}/api/ext/sessions`);
    if (!resp.ok) throw new Error('HTTP ' + resp.status);
    const data = await resp.json();
    const items = Array.isArray(data.items) ? data.items : [];
    _remoteIndex = items.map((i) => ({
      sessionId: String(i.sessionId || ''),
      title: String(i.title || ''),
      msgCount: Number(i.msgCount) || 0,
      createdAt: parseTs(i.createdAt),
      updatedAt: parseTs(i.updatedAt)
    })).filter((i) => i.sessionId);
    _remoteIndexAt = Date.now();
    return _remoteIndex;
  } catch (e) {
    console.warn('[Absorb] 桌面端索引拉取失败（按本地历史展示）:', e);
    _remoteIndex = null;
    return null;
  }
}

// getRemoteIndex 当前缓存索引（未拉取时为空数组，渲染层直接归并）
export function getRemoteIndex() {
  return _remoteIndex || [];
}

// remoteOnlyStubs 远端有而本地缺的索引条目（历史列表归并数据源）。
// 归并键 = sessionId；本地已有（含已懒加载注入的）优先。
export function remoteOnlyStubs() {
  const local = new Set(
    state.sessions.map((s) => s.sessionId).filter(Boolean));
  return getRemoteIndex().filter((r) => !local.has(r.sessionId));
}

// ---------- 远端会话懒加载 ----------

// loadRemoteSession 点击远端条目时拉取原生对象并注入本地缓存。
// 成功返回本地会话对象（fromRemote 标记），失败返回 null（调用方提示）。
export async function loadRemoteSession(sessionId) {
  try {
    const port = await getBridgePort();
    if (!port) return null;
    const resp = await fetch(
      `http://localhost:${port}/api/ext/sessions/${encodeURIComponent(sessionId)}`);
    if (!resp.ok) return null;
    const data = await resp.json();
    const raw = data.session;
    if (!raw || !Array.isArray(raw.messages)) return null;
    const s = {
      id: uid(),             // 本地键重生成；远端归并继续以 sessionId 为准
      sessionId: raw.sessionId || sessionId,
      quotes: Array.isArray(raw.quotes) ? raw.quotes : [],
      messages: raw.messages,
      createdAt: raw.createdAt || parseTs(data.createdAt),
      updatedAt: raw.updatedAt || parseTs(data.updatedAt),
      fromRemote: true
    };
    injectRemoteSession(s);
    await saveState();
    return s;
  } catch (e) {
    console.warn('[Absorb] 远端会话详情拉取失败:', e);
    return null;
  }
}

// injectRemoteSession 注入本地缓存（同 sessionId 并发加载去重 + 缓存上限驱逐）。
function injectRemoteSession(s) {
  const exists = state.sessions.find((x) => x.sessionId === s.sessionId);
  if (exists) return exists;
  if (state.sessions.length >= SESSION_CACHE_LIMIT) {
    // 优先驱逐远端缓存详情；无远端缓存可逐时回退最旧的非当前会话
    // （本地会话均已镜像，重灌零损失；从未提问的空会话无保留价值）
    for (let i = state.sessions.length - 1; i >= 0; i--) {
      const x = state.sessions[i];
      if (x.fromRemote && x.id !== state.currentSessionId) {
        state.sessions.splice(i, 1);
        break;
      }
    }
    if (state.sessions.length >= SESSION_CACHE_LIMIT) {
      for (let i = state.sessions.length - 1; i >= 0; i--) {
        if (state.sessions[i].id !== state.currentSessionId) {
          state.sessions.splice(i, 1);
          break;
        }
      }
    }
  }
  state.sessions.unshift(s);
  return s;
}

// ---------- 通用 ----------

// parseTs 服务端 ISO 串 / 扩展端毫秒数 → 毫秒时间戳；非法回退当前时间。
function parseTs(v) {
  if (typeof v === 'number' && v > 0) return v;
  if (typeof v === 'string') {
    const t = Date.parse(v);
    if (!Number.isNaN(t)) return t;
  }
  return Date.now();
}
