// Absorb 阅读助手 - 文档工具栏（PDF 解读 PR4，原 PR3 文档卡改造）
// 职责：识别当前活动标签页的文档类型 → 渲染输入框上方的单行工具栏（文件名 +
// 状态 badge + 字数 + 展开箭头 + 关闭），⌄ 展开按状态分发详情面板（文档信息/
// 建议问题/读取进度/错误重试），调 /api/page/prepare 提取正文并透传 documentId。
// 状态模型：idle（无记录）/ reading（内存态，面板关闭即中断）/ ready / error
// （终态持久化在 DOCCARDS_KEY，卡片按 url 索引，documentId 记服务端
// documents.id UUID；旧记录无 documentId 时提问链路回退传 URL，服务端兼容）。
// UI 态：_expanded（展开面板）、_dismissed（× 仅隐藏当前展示，不删提取记录，
// 切换标签页/重新导航后恢复）。
// 多文档类型扩展：DOC_TYPES 注册表（URL 识别 + 图标 + 建议问题），新类型加一项
// 即可，渲染逻辑不感知具体格式。
import { DOCCARDS_KEY } from './config.js';
import { prepareDocument } from './net.js';
import { t } from './i18n.js';

// ---------- 文档类型注册表 ----------
// ext 按 URL 路径后缀识别（查询串/锚点前）。浏览器内置查看器的 tab.url 保留
// 原始 https 地址，content script 注入不可行（查看器非普通 DOM 文档），因此
// 识别只看 URL 后缀——服务端按 content-type 兜底分类，误判无副作用。
export const DOC_TYPES = {
  pdf: {
    key: 'pdf',
    ext: /\.pdf$/i,
    icon: '../icons/PDF.png',
  },
  // 未来 docx / epub / html 等类型：在此各加一项（icon + ext）即可——建议提问
  // 由服务端章节树双通道生成（doc-suggestions-design.md），渲染不感知格式。
};

// URL → 类型定义（未命中返回 null）。isPdfUrl 保留给 conversation.js 提问链路。
export function detectDocType(url) {
  try {
    const u = new URL(url);
    if (!/^https?:$/.test(u.protocol)) return null;
    for (const k of Object.keys(DOC_TYPES)) {
      if (DOC_TYPES[k].ext.test(u.pathname)) return DOC_TYPES[k];
    }
    return null;
  } catch {
    return null;
  }
}

export function isPdfUrl(url) {
  return detectDocType(url)?.key === 'pdf';
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
let _cards = {}; // url → { url, documentId, title, state, serverStatus, charCount, truncated, message, updatedAt }

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
// （ready/partial，服务端 Injectable 同口径）时返回服务端 documentId
// （documents.id UUID）；旧记录（M0 前后端语义 documentId=URL）无该字段，
// 回退传 URL——服务端问答链路按 ID 查无时会按 URL 兼容。
// 失败态不透传——后端查无落库正文只会徒增一次查询日志。
export async function readyDocumentId(url) {
  const r = _cards[url];
  if (r && r.state === 'ready' && (r.serverStatus === 'ready' || r.serverStatus === 'partial')) {
    return r.documentId || url;
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

// ---------- UI 态（均内存态，不持久化） ----------
let _expanded = false;    // 面板是否展开
let _dismissed = null;    // × 隐藏的 URL（仅隐藏展示；切到其他 URL 后自动失效）

// ---------- 渲染 ----------
let _container = null;
let _miniEl = null;   // × 隐藏态的迷你圆标挂载点（composer 操作行内，可缺省）
let _current = null;  // { url, title } 当前活动文档标签页；null = 非文档，工具栏隐藏
let _ask = null;      // main.js 注入的提问入口（(text) => void，落输入框并提交）

function el(tag, cls, text) {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined) n.textContent = text;
  return n;
}

// 文档类型图标（扩展自带，红色圆角方块白字样式）
function fileIcon(type) {
  const img = document.createElement('img');
  img.className = 'db-fileicon';
  img.src = type.icon;
  img.alt = '';
  img.setAttribute('aria-hidden', 'true');
  return img;
}

// 状态 badge：橙色=待读取/需登录，蓝色=进行中，绿色=就绪，灰色=其他失败
function badge(text, tone) {
  return el('span', `db-badge db-badge-${tone}`, text);
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

// 工具栏行内小按钮（展开箭头 / 关闭 ×）
function barBtn(cls, label, svgPath) {
  const b = document.createElement('button');
  b.type = 'button';
  b.className = `db-btn ${cls}`;
  b.title = label;
  b.setAttribute('aria-label', label);
  b.innerHTML = `<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${svgPath}</svg>`;
  return b;
}

// 由持久化/读取态推导四态：reading（内存优先）> ready > error > idle
function docState(url) {
  if (_reading && _reading.url === url) return 'reading';
  const rec = _cards[url];
  if (rec && rec.state === 'ready') return 'ready';
  if (rec && rec.state === 'error') return 'error';
  return 'idle';
}

// 收起态单行工具栏：图标 | 文件名 | badge | 字数 | ⌄ | ×
function renderBar(type, name, state) {
  const url = _current.url;
  const rec = _cards[url];
  const bar = el('div', 'db-bar');

  bar.append(fileIcon(type));
  const nameEl = el('span', 'db-name', name);
  nameEl.title = name;
  bar.append(nameEl);

  let tone = 'orange';
  let text = t('doccard.badgeIdle');
  if (state === 'reading') { tone = 'blue'; text = t('doccard.badgeReading'); }
  else if (state === 'ready') { tone = 'green'; text = t('doccard.badgeReady'); }
  else if (state === 'error') {
    const [key, tn] = ERROR_BADGES[rec.serverStatus] || ERROR_BADGES.network_error;
    tone = tn; text = t(`doccard.${key}`);
  }
  bar.append(badge(text, tone));

  if (state === 'ready') {
    bar.append(el('span', 'db-chars', t('docbar.chars', { chars: (rec.charCount || 0).toLocaleString() })));
  }

  // 未读取态：主入口直接放工具栏行内（收起时也要一眼可见）
  if (state === 'idle') {
    const read = el('button', 'db-bar-cta', t('doccard.read'));
    read.type = 'button';
    read.addEventListener('click', () => startRead());
    bar.append(read);
  }

  const toggle = barBtn('db-toggle', _expanded ? t('docbar.collapse') : t('docbar.expand'),
    '<path d="m6 9 6 6 6-6"/>');
  toggle.setAttribute('aria-expanded', String(_expanded));
  toggle.addEventListener('click', () => { _expanded = !_expanded; render(); });
  bar.append(toggle);

  const close = barBtn('db-close', t('docbar.hide'), '<path d="M18 6 6 18M6 6l12 12"/>');
  close.addEventListener('click', () => {
    _dismissed = url;
    _expanded = false;
    render();
  });
  bar.append(close);

  return bar;
}

// 展开面板：按状态分发（信息行/建议问题/读取进度/错误重试）
function renderPanel(type, name, state) {
  const url = _current.url;
  const rec = _cards[url];
  const panel = el('div', 'db-panel');

  // ---------- 读取中 ----------
  if (state === 'reading') {
    const steps = el('div', 'db-steps');
    [t('doccard.step1'), t('doccard.step2'), t('doccard.step3')].forEach((label, i) => {
      const stepNo = i + 1;
      const row = el('div', `db-step${stepNo === _reading.step ? ' active' : stepNo < _reading.step ? ' done' : ''}`);
      row.append(el('span', 'db-stepnum', String(stepNo)), el('span', 'db-steplabel', label));
      steps.appendChild(row);
    });
    panel.append(steps, el('div', 'db-hint', t('doccard.processing')));
    return panel;
  }

  // ---------- 已就绪（ready/partial）：文档信息 + 建议问题 + 操作 ----------
  if (state === 'ready') {
    const partial = rec.serverStatus === 'partial';
    panel.append(el('div', 'db-sec', t('docbar.infoTitle')));
    const info = el('div', 'db-info');
    info.append(
      infoRow(t('docbar.rowName'), name),
      infoRow(t('docbar.rowChars'), t('docbar.chars', { chars: (rec.charCount || 0).toLocaleString() })),
      infoRow(t('docbar.rowStatus'), partial ? t('docbar.statusPartial') : t('docbar.statusReady'))
    );
    panel.append(info);

    // 动态建议提问（doc-suggestions-design.md）：服务端章节树双通道直出成句
    // 问题，原样显示、点击原样发送（documentId 由 getActivePage 自动附带）。
    // 素材不足（<2 条）不渲染该小节——裁决：无固定兜底。
    const suggestions = Array.isArray(rec.suggestions) ? rec.suggestions : [];
    if (suggestions.length >= 2) {
      panel.append(el('div', 'db-sec', t('doccard.suggestTitle')));
      const list = el('div', 'db-suggest');
      suggestions.forEach((question) => {
        const item = el('button', 'db-suggest-item');
        item.type = 'button';
        item.append(el('span', 'db-suggest-text', question), el('span', 'arr', '→'));
        item.addEventListener('click', () => { if (_ask) _ask(question); });
        list.appendChild(item);
      });
      panel.append(list);
    }

    const actions = el('div', 'db-actions');
    const reread = el('button', 'db-act-btn', t('doccard.reread'));
    reread.type = 'button';
    reread.addEventListener('click', () => startRead());
    actions.append(reread);
    panel.append(actions);
    return panel;
  }

  // ---------- 失败态：原因说明 + 重试 ----------
  if (state === 'error') {
    panel.append(el('div', 'db-desc', rec.message || t('doccard.errorGeneric')));
    const actions = el('div', 'db-actions');
    const retry = el('button', 'db-cta', t('doccard.retry'));
    retry.type = 'button';
    retry.addEventListener('click', () => startRead());
    actions.appendChild(retry);
    panel.append(actions);
    return panel;
  }

  // ---------- 未读取（idle）：说明 + 读取主按钮 ----------
  panel.append(el('div', 'db-desc', t('doccard.descIdle')));
  const actions = el('div', 'db-actions');
  const read = el('button', 'db-cta', t('doccard.read'));
  read.type = 'button';
  read.addEventListener('click', () => startRead());
  actions.appendChild(read);
  panel.append(actions);
  return panel;
}

function infoRow(k, v) {
  const row = el('div', 'db-row');
  row.append(el('span', 'db-row-k', k), el('span', 'db-row-v', v));
  return row;
}

// 迷你圆标（× 隐藏态渲染进 composer 操作行；点击恢复完整工具栏）
function renderMini(type) {
  if (!_miniEl) return;
  _miniEl.hidden = false;
  _miniEl.textContent = '';
  const mini = document.createElement('button');
  mini.type = 'button';
  mini.className = 'db-mini';
  mini.title = t('docbar.restore');
  mini.setAttribute('aria-label', t('docbar.restore'));
  mini.append(fileIcon(type));
  mini.addEventListener('click', () => { _dismissed = null; render(); });
  _miniEl.append(mini);
}

function clearMini() {
  if (!_miniEl) return;
  _miniEl.hidden = true;
  _miniEl.textContent = '';
}

function render() {
  if (!_container || !_current) return;
  const url = _current.url;
  const { title } = _current;
  const type = detectDocType(url) || DOC_TYPES.pdf;
  const name = title || filenameFromUrl(url);
  const state = docState(url);

  // × 隐藏态：完整工具栏隐藏，迷你圆标进 composer 操作行
  if (_dismissed === url) {
    _container.hidden = true;
    _container.textContent = '';
    _container.classList.remove('expanded');
    renderMini(type);
    return;
  }

  clearMini();
  _container.textContent = '';
  _container.hidden = false;
  _container.classList.toggle('expanded', _expanded);
  _container.append(renderBar(type, name, state));
  // 展开面板：收起态不渲染内容（动画由 .db-expand 的 grid-rows 过渡承担）
  const expand = el('div', 'db-expand');
  const clip = el('div');
  if (_expanded) clip.appendChild(renderPanel(type, name, state));
  expand.appendChild(clip);
  _container.append(expand);
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
        documentId: res.documentId || '',
        title: (res.title || title || filenameFromUrl(url)),
        state: 'ready',
        serverStatus: status,
        charCount: res.charCount || 0,
        truncated: !!res.truncated,
        suggestions: Array.isArray(res.suggestions) ? res.suggestions : [],
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
    // 读取期间用户可能已切走标签页：仅当仍在同一文档上才刷新展示
    if (_current && _current.url === url) {
      // 读取成功自动展开一次引导（见方案决策 2）；用户可手动收起，下次读取不再打扰
      if (_cards[url] && _cards[url].state === 'ready') _expanded = true;
      render();
    }
  }
}

// ---------- 标签页跟踪 ----------
async function refresh() {
  if (!_container) return;
  try {
    const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
    const type = tab && tab.url ? detectDocType(tab.url) : null;
    if (tab && tab.url && type) {
      // 读取完成回填过服务端标题（res.title），但展示名优先用标签页实时标题
      const prevUrl = _current && _current.url;
      _current = { url: tab.url, title: tab.title || '' };
      // 活动文档变化（含从非文档页切回）：× 隐藏自动失效；同一文档原地刷新
      // （如标题更新）不恢复，避免误触 × 后立刻复显
      if (prevUrl !== tab.url) _dismissed = null;
      if (_dismissed === tab.url) {
        render();
        return;
      }
    } else {
      _current = null;
      _container.hidden = true;
      _container.textContent = '';
      clearMini();
      return;
    }
  } catch {
    _current = null;
    _container.hidden = true;
    clearMini();
    return;
  }
  render();
}

// init（main.js 调用）：注入提问入口、恢复持久化状态、挂标签页监听。
// ask(text)：main.js 提供——把建议问题追加进输入框并走 submitQuestion 全链路。
export async function initDocCard({ ask }) {
  _ask = ask || null;
  _container = document.getElementById('docCard');
  _miniEl = document.getElementById('docMiniSlot');
  if (!_container) return;
  await loadCards();
  await refresh();
  // 切换/导航标签页实时刷新工具栏（SW 不参与，监听在面板页自身上下文）
  chrome.tabs.onActivated.addListener(() => { refresh(); });
  chrome.tabs.onUpdated.addListener((_tabId, info) => {
    if (info.url || info.title) refresh();
  });
}
