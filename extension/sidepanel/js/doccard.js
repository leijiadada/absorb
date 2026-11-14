// Absorb 阅读助手 - 文档卡（PDF 解读 PR3）
// 职责：识别当前活动标签页的 PDF 文档 → 渲染三态文档卡（未读取/读取中/已就绪 +
// 失败态），调 /api/page/prepare 提取正文并把 documentId 透传给提问链路。
// 状态模型：idle（无记录）/ reading（内存态，面板关闭即中断）/ ready / error
// （终态持久化在 DOCCARDS_KEY，documentId = URL，与服务端锚点一致）。
import { DOCCARDS_KEY } from './config.js';
import { prepareDocument } from './net.js';
import { t } from './i18n.js';

// PDF URL 识别：路径以 .pdf 结尾（查询串/锚点前）。浏览器内置查看器的 tab.url
// 保留原始 https 地址，content script 注入不可行（查看器非普通 DOM 文档），
// 因此识别只看 URL 后缀——服务端按 content-type 兜底分类，误判无副作用。
export function isPdfUrl(url) {
  try {
    const u = new URL(url);
    return /^https?:$/.test(u.protocol) && /\.pdf$/i.test(u.pathname);
  } catch {
    return false;
  }
}

// 从 URL 取文件名（读取前 tab.title 可能就是 URL 本身）
function filenameFromUrl(url) {
  try {
    const p = decodeURIComponent(new URL(url).pathname);
    return p.slice(p.lastIndexOf('/') + 1) || p;
  } catch {
    return url;
  }
}

// ---------- 状态表（持久化终态） ----------
let _cards = {}; // documentId(=URL) → { url, title, state, serverStatus, charCount, truncated, message, updatedAt }

async function loadCards() {
  try {
    const d = await chrome.storage.local.get(DOCCARDS_KEY);
    _cards = (d[DOCCARDS_KEY] && typeof d[DOCCARDS_KEY] === 'object') ? d[DOCCARDS_KEY] : {};
  } catch (e) {
    console.warn('[Absorb] 文档卡状态读取失败:', e);
    _cards = {};
  }
}

async function saveCards() {
  // 只保留最近 20 份文档，防 storage 膨胀（记录本身只有元信息，很小）
  const entries = Object.values(_cards)
    .sort((a, b) => (b.updatedAt || 0) - (a.updatedAt || 0))
    .slice(0, 20);
  _cards = Object.fromEntries(entries.map((r) => [r.url, r]));
  try {
    await chrome.storage.local.set({ [DOCCARDS_KEY]: _cards });
  } catch (e) {
    console.error('[Absorb] 文档卡状态保存失败:', e);
  }
}

// 提问链路取 documentId（conversation.js 调用）：该 URL 已提取到正文
// （ready/partial，服务端 Injectable 同口径）时返回 URL 本身，否则空串。
// 失败态不透传——后端查无落库正文只会徒增一次查询日志。
export async function readyDocumentId(url) {
  const r = _cards[url];
  if (r && r.state === 'ready' && (r.serverStatus === 'ready' || r.serverStatus === 'partial')) {
    return url;
  }
  return '';
}

// ---------- 读取中（内存态） ----------
let _reading = null; // { url, step: 1|2|3 } 同时只允许一份文档在读取
let _stepTimers = [];

function clearReadingTimers() {
  _stepTimers.forEach(clearTimeout);
  _stepTimers = [];
}

// 步骤推进模拟：服务端不细分阶段（校验/下载/提取在一个请求里），前端按
// 经验耗时推进展示步——发起即步骤1，1.2s 后步骤2（下载中，覆盖绝大多数
// 时长），8s 仍未返回推进步骤3（大文件提取）。纯 UI 展示，不影响结果。
function startReading(url) {
  _reading = { url, step: 1 };
  _stepTimers = [
    setTimeout(() => { if (_reading && _reading.url === url) { _reading.step = 2; render(); } }, 1200),
    setTimeout(() => { if (_reading && _reading.url === url) { _reading.step = 3; render(); } }, 8000)
  ];
}

// ---------- 渲染 ----------
let _container = null;
let _current = null;  // { url, title } 当前活动 PDF 标签页；null = 非 PDF，卡片隐藏
let _ask = null;      // main.js 注入的提问入口（(text) => void，落输入框并提交）

function el(tag, cls, text) {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined) n.textContent = text;
  return n;
}

// PDF 文件图标（扩展自带 PDF.png，红色圆角方块白字样式）
function fileIcon() {
  const img = document.createElement('img');
  img.className = 'dc-fileicon';
  img.src = '../icons/PDF.png';
  img.alt = '';
  img.setAttribute('aria-hidden', 'true');
  return img;
}

// 状态 badge：橙色=待读取/需登录，蓝色=进行中，绿色=就绪，灰色=其他失败
function badge(text, tone) {
  return el('span', `dc-badge dc-badge-${tone}`, text);
}

// 失败态 badge 文案与配色：needs_auth 橙（可自助解决），其余灰
const ERROR_BADGES = {
  needs_auth: ['badgeNeedAuth', 'orange'],
  too_large: ['badgeTooLarge', 'gray'],
  no_text: ['badgeNoText', 'gray'],
  encrypted: ['badgeEncrypted', 'gray'],
  invalid_url: ['badgeInvalidUrl', 'gray'],
  network_error: ['badgeError', 'gray']
};

function render() {
  if (!_container || !_current) return;
  const { url, title } = _current;
  const name = title || filenameFromUrl(url);
  const rec = _cards[url];
  _container.textContent = '';

  // ---------- 读取中 ----------
  if (_reading && _reading.url === url) {
    const head = el('div', 'dc-head');
    head.append(fileIcon(), el('span', 'dc-title dc-title-inline', name), badge(t('doccard.badgeReading'), 'blue'));
    _container.appendChild(head);
    const steps = el('div', 'dc-steps');
    [t('doccard.step1'), t('doccard.step2'), t('doccard.step3')].forEach((label, i) => {
      const stepNo = i + 1;
      const row = el('div', `dc-step${stepNo === _reading.step ? ' active' : stepNo < _reading.step ? ' done' : ''}`);
      row.append(el('span', 'dc-stepnum', String(stepNo)), el('span', 'dc-steplabel', label));
      steps.appendChild(row);
    });
    _container.appendChild(steps);
    _container.appendChild(el('div', 'dc-hint', t('doccard.processing')));
    return;
  }

  // ---------- 已就绪（ready/partial） ----------
  if (rec && rec.state === 'ready') {
    const partial = rec.serverStatus === 'partial';
    const head = el('div', 'dc-head');
    head.append(fileIcon(), el('span', 'dc-title dc-title-inline', name), badge(t('doccard.badgeReady'), 'green'));
    _container.appendChild(head);
    const chars = (rec.charCount || 0).toLocaleString();
    _container.appendChild(el('div', 'dc-meta',
      t(partial ? 'doccard.metaPartial' : 'doccard.metaReady', { chars })));
    _container.appendChild(el('div', 'dc-divider'));
    _container.appendChild(el('div', 'dc-suggest-title', t('doccard.suggestTitle')));
    const list = el('div', 'dc-suggest');
    ['suggest1', 'suggest2', 'suggest3'].forEach((k) => {
      const item = el('button', 'dc-suggest-item');
      item.type = 'button';
      item.append(el('span', 'dc-suggest-text', t(`doccard.${k}`)), el('span', 'arr', '↗'));
      item.addEventListener('click', () => {
        // 建议问题透传 documentId：提问时 getActivePage 按当前活动 PDF 标签页
        // 自动附带，这里只负责把问题落输入框提交
        if (_ask) _ask(t(`doccard.${k}`));
      });
      list.appendChild(item);
    });
    _container.appendChild(list);
    const actions = el('div', 'dc-actions');
    const reread = el('button', 'dc-action-btn', t('doccard.reread'));
    reread.type = 'button';
    reread.addEventListener('click', () => startRead());
    const open = el('button', 'dc-action-btn', t('doccard.openOriginal'));
    open.type = 'button';
    open.addEventListener('click', () => chrome.tabs.create({ url }));
    actions.append(reread, open);
    _container.appendChild(actions);
    return;
  }

  // ---------- 失败态（效果图未给出，按 5.1 布局变体：badge 按状态配色、
  // 说明区渲染 message、按钮改重试） ----------
  if (rec && rec.state === 'error') {
    const [key, tone] = ERROR_BADGES[rec.serverStatus] || ERROR_BADGES.network_error;
    const head = el('div', 'dc-head');
    head.append(fileIcon(), el('span', 'dc-title dc-title-inline', name), badge(t(`doccard.${key}`), tone));
    _container.appendChild(head);
    _container.appendChild(el('div', 'dc-meta', t('doccard.metaSource')));
    _container.appendChild(el('div', 'dc-divider'));
    _container.appendChild(el('div', 'dc-desc', rec.message || t('doccard.errorGeneric')));
    const foot = el('div', 'dc-foot');
    foot.appendChild(el('span', 'dc-chip', 'PDF'));
    const retry = el('button', 'dc-cta', t('doccard.retry'));
    retry.type = 'button';
    retry.addEventListener('click', () => startRead());
    foot.appendChild(retry);
    _container.appendChild(foot);
    return;
  }

  // ---------- 未读取（idle） ----------
  const head = el('div', 'dc-head');
  head.append(fileIcon(), el('span', 'dc-label', t('doccard.label')), badge(t('doccard.badgeIdle'), 'orange'));
  _container.appendChild(head);
  _container.appendChild(el('div', 'dc-title', name));
  _container.appendChild(el('div', 'dc-meta', t('doccard.metaSource')));
  _container.appendChild(el('div', 'dc-divider'));
  _container.appendChild(el('div', 'dc-desc', t('doccard.descIdle')));
  const foot = el('div', 'dc-foot');
  foot.appendChild(el('span', 'dc-chip', 'PDF'));
  const read = el('button', 'dc-cta', t('doccard.read'));
  read.type = 'button';
  read.addEventListener('click', () => startRead());
  foot.appendChild(read);
  _container.appendChild(foot);
}

// ---------- 读取流程 ----------
async function startRead() {
  if (!_current || _reading) return;
  const { url, title } = _current;
  startReading(url);
  render();
  try {
    const res = await prepareDocument(url, title);
    const status = res && res.status;
    if (status === 'ready' || status === 'partial') {
      _cards[url] = {
        url,
        title: (res.title || title || filenameFromUrl(url)),
        state: 'ready',
        serverStatus: status,
        charCount: res.charCount || 0,
        truncated: !!res.truncated,
        updatedAt: Date.now()
      };
    } else {
      _cards[url] = {
        url, title,
        state: 'error',
        serverStatus: status || 'network_error',
        message: (res && res.message) || '',
        updatedAt: Date.now()
      };
    }
  } catch (e) {
    // 桌面端未就绪/网络失败/10 分钟超时（AbortError）统一按错误态落卡，可重试
    _cards[url] = {
      url, title,
      state: 'error',
      serverStatus: 'network_error',
      message: e && e.name === 'AbortError' ? t('doccard.errorTimeout') : (e && e.message) || '',
      updatedAt: Date.now()
    };
  } finally {
    clearReadingTimers();
    _reading = null;
    await saveCards();
    // 读取期间用户可能已切走标签页：仅当仍在同一 PDF 上才刷新展示
    if (_current && _current.url === url) render();
  }
}

// ---------- 标签页跟踪 ----------
async function refresh() {
  if (!_container) return;
  try {
    const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
    if (tab && tab.url && isPdfUrl(tab.url)) {
      // 读取完成回填过服务端标题（res.title），但展示名优先用标签页实时标题
      _current = { url: tab.url, title: tab.title || '' };
      _container.hidden = false;
    } else {
      _current = null;
      _container.hidden = true;
      _container.textContent = '';
      return;
    }
  } catch {
    _current = null;
    _container.hidden = true;
    return;
  }
  render();
}

// init（main.js 调用）：注入提问入口、恢复持久化状态、挂标签页监听。
// ask(text)：main.js 提供——把建议问题追加进输入框并走 submitQuestion 全链路。
export async function initDocCard({ ask }) {
  _ask = ask || null;
  _container = document.getElementById('docCard');
  if (!_container) return;
  await loadCards();
  await refresh();
  // 切换/导航标签页实时刷新卡片（SW 不参与，监听在面板页自身上下文）
  chrome.tabs.onActivated.addListener(() => { refresh(); });
  chrome.tabs.onUpdated.addListener((_tabId, info) => {
    if (info.url || info.title) refresh();
  });
}
