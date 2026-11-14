// Absorb 阅读助手 - 渲染层
// 职责：DOM 引用、消息/引用条/历史列表渲染、滚动控制、提问按钮状态、toast 提示
import { state, currentSession, getStreaming, saveState, saveDraftSoon, composerImages } from './state.js';
import { remoteOnlyStubs, loadRemoteSession } from './sync.js';
import { keepContextFrom, attachKnowledgeEntry } from './knowledge.js';
import { t } from './i18n.js';
import { renderVisualBlock } from './visual/renderer.js';
import { buildVisualContent } from './visual/blocks.js';
import { attachFigToolbar } from './figToolbar.js';

// 初始化 Markdown 渲染（marked + DOMPurify 已通过 <script> 加载为全局变量）
marked.setOptions({ breaks: true, gfm: true });

// 代码块复制按钮图标（SVG，跟随 currentColor，由 .code-copy 颜色控制）
export const ICON_COPY = '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M9 9h10a1 1 0 0 1 1 1v10a1 1 0 0 1-1 1H9a1 1 0 0 1-1-1V10a1 1 0 0 1 1-1Z"/><path d="M5 15H4a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1h10a1 1 0 0 1 1 1v1"/></svg>';
export const ICON_CHECK = '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><polyline points="20 6 9 17 4 12"/></svg>';
export const ICON_IMAGE = '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="3" width="18" height="18" rx="2"/><circle cx="8.5" cy="8.5" r="1.5"/><path d="M21 15l-5-5L5 21"/></svg>';

// 语法高亮：覆写 code 渲染器，产出色词 <span class="hljs-…">（DOMPurify 默认保留 class 属性）
// lang 缺失/无匹配语言时退化为自动识别；识别失败按纯文本转义，保证永不引入未转义 HTML。
function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'
  })[c]);
}
// 流式帧高亮开关：false 时代码只转义不上色（hljs 是全量重渲的最大开销，流式期间跳过），
// 由 renderMarkdown 设定并在解析后复位
let _highlightCode = true;

marked.use({
  renderer: {
    code({ text, lang }) {
      const language = (lang || '').toLowerCase();
      const cls = language ? `hljs language-${language}` : 'hljs';
      let html;
      if (!_highlightCode) {
        html = escapeHtml(text);
      } else {
        try {
          if (language && hljs.getLanguage(language)) {
            html = hljs.highlight(text, { language }).value;
          } else {
            html = hljs.highlightAuto(text).value;
          }
        } catch {
          html = escapeHtml(text);
        }
      }
      // 结构：<pre> > [工具条(语言标签 + 复制按钮)] + <code>
      // 工具条独占一行、按钮右上角；复制内容取 <code> 的文本（事件委托见 main.js）
      const langLabel = language || '';
      return `<pre class="code-block"><div class="code-toolbar">` +
        (langLabel ? `<span class="code-lang">${escapeHtml(langLabel)}</span>` : '') +
        `<button class="code-copy" type="button" tabindex="-1" title="${t('copy.copy')}" aria-label="${t('copy.copy')}">${ICON_COPY}</button>` +
        `</div><code class="${cls}">${html}</code></pre>`;
    }
  }
});

// 渲染 Markdown（供流式增量/历史重建共用同一套逻辑）
// opts.highlight=false 供流式帧使用：跳过 hljs 只转义，终帧与历史重建默认高亮
export function renderMarkdown(content, sessionId, opts = {}) {
  _highlightCode = opts.highlight !== false;
  try {
    const html = DOMPurify.sanitize(marked.parse(String(content || '')));
    const tmp = document.createElement('div');
    tmp.innerHTML = html;
    return tmp.innerHTML;
  } finally {
    _highlightCode = true;
  }
}

// ---------- Visual Explanation 接入（胶水层） ----------
// 助手回答正文渲染：切分 :::absorb-visual 块（契约见 visual/SPEC.md）与普通 Markdown，
// 各自渲染后按原文顺序拼装为 DocumentFragment。分段编排与失败态（流式占位 / 渲染降级）
// 在 visual/blocks.js（零 Absorb 依赖、可独立单测），这里只做依赖绑定与 DOM 装配。
// opts.final=false（流式帧）：可视化块只显示占位提示——流式期间不解析（未闭合块必然
// 解析不完整），且占位比半渲染页面稳定；opts.final=true（终帧/历史重建）：解析渲染，
// 单块异常降级为该块原始 Markdown（失败安全，不吞内容、不阻塞回答）。
export function renderAssistantContent(content, sessionId, opts = {}) {
  const final = opts.final === true;
  const visualErrors = [];
  const mdToHtml = (text) => renderMarkdown(text, sessionId, opts);
  const nodes = buildVisualContent(
    content,
    final,
    { el: elNode, mdToHtml, text: t },
    (seg, toHtml, errors) => renderVisualBlock(seg, toHtml, errors),
    visualErrors
  );
  const frag = document.createDocumentFragment();
  for (const node of nodes) frag.appendChild(node);
  if (final && visualErrors.length) {
    console.debug('[Absorb visual] 可视化解析告警:', visualErrors);
  }
  return frag;
}

// 依赖注入用的小节点工厂（className 可选）
function elNode(tag, cls) {
  const node = document.createElement(tag);
  if (cls) node.className = cls;
  return node;
}

// ---------- Mermaid 图表渲染 ----------
// 回答里的 ```mermaid 代码块在最终渲染（流式结束/历史重建）时替换为 SVG 图；
// 流式期间按普通代码块展示（语法不完整时渲染必失败）。渲染失败保持代码块原样，
// 用户仍可阅读/复制源码——失败安全，不阻塞回答。
// securityLevel: 'strict' 禁用 HTML 标签与 click 回调（mermaid 内置 DOMPurify 清洗
// label，防参考材料 prompt injection 注入恶意标签），这是硬安全配置。
let _mermaidLoading = null;
let _mermaidSeq = 0;

function loadMermaid() {
  if (window.mermaid) return Promise.resolve();
  if (!_mermaidLoading) {
    // 懒加载：首次遇到 mermaid 块才注入 vendor 脚本（2.8MB，避免拖慢面板首开）
    _mermaidLoading = new Promise((resolve, reject) => {
      const s = document.createElement('script');
      s.src = 'vendor/mermaid.min.js';
      s.onload = () => {
        try {
          window.mermaid.initialize({ startOnLoad: false, securityLevel: 'strict', theme: 'default' });
          resolve();
        } catch (e) { reject(e); }
      };
      s.onerror = () => { _mermaidLoading = null; reject(new Error('mermaid 加载失败')); };
      document.head.appendChild(s);
    });
  }
  return _mermaidLoading;
}

// 将 root 内 lang=mermaid 的代码块异步替换为 SVG。
// 已处理/替换成功的块打 dataset 标记，重复调用（增量重渲染）不重复渲染。
export function decorateMermaid(root) {
  if (!root) return;
  const blocks = [...root.querySelectorAll('.code-block')].filter((pre) => {
    if (pre.dataset.mermaidDone) return false;
    const lang = pre.querySelector('.code-lang');
    return lang && lang.textContent.trim().toLowerCase() === 'mermaid';
  });
  if (blocks.length === 0) return;
  loadMermaid().then(() => {
    for (const pre of blocks) {
      const code = pre.querySelector('code');
      const src = code ? code.textContent : '';
      if (!src.trim()) continue;
      const id = `mmd-${Date.now()}-${_mermaidSeq++}`;
      // 兼容两种构建形态：本 dist 的 render 同步返回 SVG 字符串；
      // Promise.resolve 包裹后字符串直接透传，Promise 则等待其完成
      Promise.resolve(window.mermaid.render(id, src)).then((res) => {
        // 兼容不同返回形态：SVG 字符串 / { svg } 对象 / DOM 容器
        const svg = typeof res === 'string' ? res
          : (res && typeof res.svg === 'string') ? res.svg
          : (res && res.innerHTML) ? res.innerHTML : null;
        if (!svg) throw new Error('render 返回不含 SVG');
        pre.dataset.mermaidDone = '1';
        const fig = document.createElement('div');
        fig.className = 'mermaid-figure';
        fig.innerHTML = svg; // strict 模式下 mermaid 输出已自清洗
        // 图卡片工具栏（复制/下载/新窗口三键）：只挂渲染成功的 SVG，失败的代码块不挂
        attachFigToolbar(fig, { kind: 'svg', svg: fig.querySelector('svg') });
        pre.replaceWith(fig);
      }).catch(() => {
        // 语法非法：清理 mermaid 遗留的临时元素，代码块保持原样（不重试）
        document.getElementById(id)?.remove();
        document.getElementById('d' + id)?.remove();
        pre.dataset.mermaidDone = '1';
      });
    }
  }).catch(() => { /* mermaid 脚本加载失败：代码块保持原样 */ });
}

// ---------- Mermaid Lightbox（全屏查看器：滚轮缩放 + 拖拽平移 + 适配重置） ----------
// 单例 DOM：点击 .mermaid-figure 弹出（main.js 事件委托调用 openLightbox），
// clone SVG 大图查看；关闭清空内容释放内存；ESC / 点空白 / ✕ 三种方式关闭。
let _lb = null;

function ensureLightbox() {
  if (_lb) return _lb;
  const el = document.createElement('div');
  el.className = 'mermaid-lightbox';
  el.setAttribute('role', 'dialog');
  el.setAttribute('aria-label', t('lightbox.close'));
  el.innerHTML =
    '<div class="lb-toolbar">' +
    `<button class="lb-btn" data-act="zoomout" type="button" title="${t('lightbox.zoomOut')}">−</button>` +
    `<button class="lb-btn" data-act="zoomin" type="button" title="${t('lightbox.zoomIn')}">＋</button>` +
    `<button class="lb-btn" data-act="reset" type="button" title="${t('lightbox.reset')}">1:1</button>` +
    `<button class="lb-btn" data-act="close" type="button" title="${t('lightbox.close')}">✕</button>` +
    '</div><div class="lb-stage"><div class="lb-holder"></div></div>';
  document.body.appendChild(el);
  const stage = el.querySelector('.lb-stage');
  const holder = el.querySelector('.lb-holder');
  const st = { el, stage, holder, scale: 1, tx: 0, ty: 0, drag: null };
  _lb = st;
  let stageMoved = false; // pointer 拖动标记：区分"拖拽"与"点击空白关闭"

  const apply = () => {
    holder.style.transform = `translate(${st.tx}px, ${st.ty}px) scale(${st.scale})`;
  };
  st.apply = apply; // 暴露给图片版查看器重置 transform（单例复用，残留会把位图顶出居中位）
  const clamp = (v, lo, hi) => Math.min(hi, Math.max(lo, v));

  // 以 stage 内 (px,py) 为中心缩放，保持指针下的内容点不动（origin=center 数学）：
  // 内容点 c=(d-t)/s，新 s2 下 t2=d-c*s2
  const zoomAt = (px, py, factor) => {
    const dx = px - stage.clientWidth / 2, dy = py - stage.clientHeight / 2;
    const s2 = clamp(st.scale * factor, 0.2, 8);
    st.tx = dx - (dx - st.tx) / st.scale * s2;
    st.ty = dy - (dy - st.ty) / st.scale * s2;
    st.scale = s2;
    apply();
  };
  st.zoomAt = zoomAt;

  // 重置：整图适配 stage（留边距），tx/ty 归零由 flex 居中完成
  st.fit = () => {
    const svg = holder.querySelector('svg');
    const vb = svg && svg.viewBox && svg.viewBox.baseVal;
    const vw = (vb && vb.width) || 800, vh = (vb && vb.height) || 600;
    if (svg) {
      svg.style.width = vw + 'px';
      svg.style.height = vh + 'px';
    }
    st.scale = Math.min(1, (stage.clientWidth - 24) / vw, (stage.clientHeight - 24) / vh);
    st.tx = 0; st.ty = 0;
    apply();
  };

  stage.addEventListener('wheel', (e) => {
    e.preventDefault();
    const rect = stage.getBoundingClientRect();
    zoomAt(e.clientX - rect.left, e.clientY - rect.top, e.deltaY < 0 ? 1.15 : 1 / 1.15);
  }, { passive: false });

  stage.addEventListener('pointerdown', (e) => {
    if (e.button !== 0) return;
    st.drag = { x: e.clientX, y: e.clientY, tx: st.tx, ty: st.ty };
    stageMoved = false;
    stage.classList.add('dragging');
    try { stage.setPointerCapture(e.pointerId); } catch { /* 旧内核忽略 */ }
  });
  stage.addEventListener('pointermove', (e) => {
    if (!st.drag) return;
    const ddx = e.clientX - st.drag.x, ddy = e.clientY - st.drag.y;
    if (Math.abs(ddx) + Math.abs(ddy) > 4) stageMoved = true;
    st.tx = st.drag.tx + ddx;
    st.ty = st.drag.ty + ddy;
    apply();
  });
  const endDrag = () => { st.drag = null; stage.classList.remove('dragging'); };
  stage.addEventListener('pointerup', endDrag);
  stage.addEventListener('pointercancel', endDrag);
  stage.addEventListener('click', () => { if (!stageMoved) closeLightbox(); stageMoved = false; });

  el.addEventListener('click', (e) => {
    const act = e.target.closest('.lb-btn')?.dataset.act;
    if (!act) return;
    if (act === 'zoomin') zoomAt(stage.clientWidth / 2, stage.clientHeight / 2, 1.3);
    else if (act === 'zoomout') zoomAt(stage.clientWidth / 2, stage.clientHeight / 2, 1 / 1.3);
    else if (act === 'reset') st.fit();
    else if (act === 'close') closeLightbox();
  });

  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && el.classList.contains('open')) closeLightbox();
  });
  return st;
}

// 打开查看器：clone 目标 SVG（去掉 mermaid 内联 max-width/width 限制，尺寸交给 fit）
export function openLightbox(svgEl) {
  const st = ensureLightbox();
  const clone = svgEl.cloneNode(true);
  clone.removeAttribute('style');
  clone.removeAttribute('width');
  clone.removeAttribute('height');
  st.holder.innerHTML = '';
  st.holder.appendChild(clone);
  st.el.classList.add('open');
  st.fit();
}

// 无原图缩略图的一次性点击提示（方案 D）：data-tip 挂在缩略行容器上、2s 自动清除，
// 气泡样式见 sidepanel.css 的 .msg-thumbs.tip-once[data-tip]（B1 禁用提示同款形态）
const tipTimers = new WeakMap();
function showThumbTip(el, text) {
  el.dataset.tip = text;
  el.classList.add('tip-once');
  clearTimeout(tipTimers.get(el));
  tipTimers.set(el, setTimeout(() => {
    delete el.dataset.tip;
    el.classList.remove('tip-once');
  }, 2000));
}

// 图片版查看器（方案 D）：复用 lightbox 遮罩与容器放 <img>（openLightbox 是 SVG 专用，
// clone/fit 按 viewBox 计算，不适用位图）。尺寸由 CSS 自适应；点击图片/空白即关闭
// （stage click 冒泡语义与 SVG 查看一致），Esc 关闭共用既有监听。
// 打开前必须重置 transform：lightbox 单例复用，上次 SVG 会话的 scale/tx/ty 残留会把
// 位图顶出居中位（stage 的 flex 居中作用在 holder 盒子上，transform 残留覆盖它）。
export function openImageLightbox(dataUrl) {
  const st = ensureLightbox();
  st.scale = 1; st.tx = 0; st.ty = 0;
  st.apply();
  const img = document.createElement('img');
  img.className = 'lightbox-img';
  img.src = dataUrl;
  img.alt = '';
  st.holder.innerHTML = '';
  st.holder.appendChild(img);
  st.el.classList.add('open');
}

function closeLightbox() {
  if (!_lb) return;
  _lb.el.classList.remove('open');
  _lb.holder.innerHTML = '';
  _lb.drag = null;
}

// ---------- DOM ----------
export const $ = (id) => document.getElementById(id);
export const els = {
  messages: $('messages'),
  emptyTip: $('emptyTip'),
  questionInput: $('questionInput'),
  askBtn: $('askBtn'),
  newSession: $('newSession'),
  toggleHistory: $('toggleHistory'),
  historyDrawer: $('historyDrawer'),
  restoreHistory: $('restoreHistory'),
  closeHistory: $('closeHistory'),
  historyList: $('historyList'),
  reconnectBar: $('reconnectBar'),
  reconnectMsg: $('reconnectMsg'),
  reconnectBtn: $('reconnectBtn'),
  desktopGuide: $('desktopGuide'),
  dgDownload: $('dgDownload'),
  dgRecheck: $('dgRecheck'),
  attachments: $('attachments'),
  imageBtn: $('imageBtn'),
  imageFile: $('imageFile')
};

// ---------- Rendering ----------
// 划词标签(cip)显示长度:超过 N 字截断并加省略号
export const CHIP_LEN = 5;
export function chipText(text) {
  const s = String(text || '');
  return s.length > CHIP_LEN ? s.slice(0, CHIP_LEN) + '…' : s;
}

// 构造一个划词标签节点（data-qid 关联 session.quotes 中的完整数据）
// readonly=true 用于气泡内只读展示：不渲染 × 删除钮（那里没有删除交互，显示会误导）
export function createChipEl(q, readonly = false) {
  const chip = document.createElement('span');
  chip.className = 'quote-chip';
  chip.setAttribute('contenteditable', 'false');
  chip.dataset.qid = String(q.id);
  const txt = document.createElement('span');
  txt.className = 'chip-text';
  txt.textContent = chipText(q.text);
  txt.title = q.text;
  if (readonly) {
    chip.append(txt);
  } else {
    const x = document.createElement('span');
    x.className = 'chip-x';
    x.textContent = '×';
    x.setAttribute('role', 'button');
    x.setAttribute('title', t('ui.removeChip'));
    chip.append(txt, x);
  }
  return chip;
}

// ---------- 输入框序列化与草稿 ----------
// 输入框内容序列化：文字原样，chip 按出现顺序 → [[n]] 占位（refs 存对应 qid 顺序表）。
// 提交（conversation.extractComposer）与草稿持久化共用，保证两处语义一致
export function serializeComposer() {
  const el = els.questionInput;
  const refs = [];
  let text = '';
  const walk = (nodes) => {
    for (const node of nodes) {
      if (node.nodeType === Node.TEXT_NODE) {
        text += node.textContent;
      } else if (node.nodeType === Node.ELEMENT_NODE) {
        if (node.classList && node.classList.contains('quote-chip')) {
          refs.push(node.dataset.qid);
          text += `[[${refs.length}]]`;
        } else {
          walk(node.childNodes);
        }
      }
    }
  };
  walk(el.childNodes);
  return { text, refs };
}

// 输入框变化后同步草稿（防抖落 storage）
export function syncDraft() {
  saveDraftSoon(JSON.stringify(serializeComposer()));
}

// 草稿回填：[[n]] → 按 refs[n-1] 的 qid 从当前会话 quotes 还原 chip（查不到的占位符丢弃），
// 其余为文字节点。在 renderComposerChips 之后调用（重放关面板前的完整输入状态）
export function restoreDraft(raw) {
  const el = els.questionInput;
  const quotes = currentSession()?.quotes || [];
  const byId = new Map(quotes.map((q) => [String(q.id), q]));
  el.textContent = '';
  for (const tok of String(raw.text).split(/(\[\[\d+\]\])/)) {
    if (!tok) continue;
    const m = tok.match(/^\[\[(\d+)\]\]$/);
    if (m) {
      const qid = raw.refs[Number(m[1]) - 1];
      const q = qid != null && byId.get(String(qid));
      if (q) el.appendChild(createChipEl(q));
    } else {
      el.appendChild(document.createTextNode(tok));
    }
  }
  updatePlaceholder();
  updateAskBtn();
  syncDraft();
}

// 在可编辑光标位置（或末尾）插入新标签，将光标固定到标签右侧以便继续打字
export function appendNewQuoteChip(q) {
  const chip = createChipEl(q);
  const sel = window.getSelection();
  const inputEl = els.questionInput;
  inputEl.focus(); // 确保焦点在输入框，否则 addRange 到未聚焦文档时光标不会落地

  // 判定插入位置：光标未聚焦/不在输入框内/在末尾 → 追加到内容末尾；否则插入光标处
  const atEnd = !sel || !sel.rangeCount || !inputEl.contains(sel.anchorNode) || isCaretAtEnd(inputEl);
  if (atEnd) {
    inputEl.appendChild(chip);
  } else {
    const range = sel.getRangeAt(0);
    range.deleteContents();
    range.insertNode(chip);
  }

  // 统一把光标放到 chip 正右侧
  const caret = document.createRange();
  caret.setStartAfter(chip);
  caret.collapse(true);
  const s = window.getSelection();
  s.removeAllRanges();
  s.addRange(caret);
  updatePlaceholder();
  updateAskBtn();
  syncDraft();
}

// 判断光标是否位于 input 末尾（或无选区）：是则视为追加场景
function isCaretAtEnd(el) {
  const sel = window.getSelection();
  if (!sel || !sel.rangeCount) return true;
  const range = sel.getRangeAt(0);
  if (!el.contains(range.commonAncestorContainer)) return true;
  const endRange = document.createRange();
  endRange.selectNodeContents(el);
  endRange.collapse(false); // 折叠到内容末尾
  // range 的 END 已到达输入框内容末尾 → 光标在末尾
  return range.compareBoundaryPoints(Range.END_TO_END, endRange) >= 0;
}

// 把光标移到 input 末尾（用于追加/清空之后聚焦继续输入）
export function placeCaretAtEnd(el) {
  const sel = window.getSelection();
  const range = document.createRange();
  range.selectNodeContents(el);
  range.collapse(false);
  sel.removeAllRanges();
  sel.addRange(range);
  el.focus();
}

// 全量重建输入框标签（切换会话/初始化时用，会清空已输入文本）
export function renderComposerChips() {
  const s = currentSession();
  const quotes = s?.quotes || [];
  const el = els.questionInput;
  el.textContent = '';
  for (const q of quotes) el.appendChild(createChipEl(q));
  updatePlaceholder();
  updateAskBtn();
  syncDraft(); // 重建态（含清空）同步进草稿，提交/新建等清空场景无需再手动清
}

// 根据输入框是否有内容（文本或标签）切换占位符与按钮可用态
export function updatePlaceholder() {
  const el = els.questionInput;
  const hasContent = el.textContent.trim().length > 0 || el.querySelector('.quote-chip');
  el.classList.toggle('has-content', hasContent);
}

function sourceLabel(url) {
  try {
    const u = new URL(url);
    return u.hostname;
  } catch {
    return url ? t('ui.webSource') : '';
  }
}

export function renderMessages() {
  const s = currentSession();
  const list = s?.messages || [];
  if (list.length === 0) {
    els.emptyTip.style.display = '';
    [...els.messages.querySelectorAll('.msg')].forEach((n) => n.remove());
    return;
  }
  els.emptyTip.style.display = 'none';
  // 简单全量重渲染
  [...els.messages.querySelectorAll('.msg')].forEach((n) => n.remove());
  for (let i = 0; i < list.length; i++) {
    const m = list[i];
    const node = buildMsgNode(m.role, m.content, false, m.context, m.reasoning, m.stopped, m.images);
    // M2:历史重建时为"完整回答"(有 requestId 且未被停止)挂知识保存入口,
    // 上下文取其前一条 user 消息(与流式路径的 userMsg 捕获语义一致)
    if (m.role === 'assistant' && m.requestId && !m.stopped && i > 0 && list[i - 1].role === 'user') {
      attachKnowledgeEntry(node, keepContextFrom(s, list[i - 1], m));
    }
    els.messages.appendChild(node);
  }
  scrollToBottom();
}

export function buildMsgNode(role, content, streaming = false, context = null, reasoning = null, stopped = false, images = null) {
  const wrap = document.createElement('div');
  wrap.className = `msg ${role}${streaming ? ' streaming' : ''}`;

  const roleEl = document.createElement('div');
  roleEl.className = 'role';
  roleEl.textContent = role === 'user' ? t('ui.me') : 'Absorb';

  const bubble = document.createElement('div');
  bubble.className = 'bubble';
  bubble.innerHTML = '';
  // 用户消息是纯文本：不走 markdown（避免 #、*、` 被误解释为格式，也无 p 默认边距虚高），
  // textContent 赋值天然转义 < >，换行由 .msg.user .bubble 的 pre-wrap 保留
  if (role === 'user') {
    // 历史缩略图（方案 D 收敛定案）：thumb 为持久化的视觉索引（知道发过什么图）；
    // full 为内存态原图（当轮/面板存续期），点击放大。原图不持久化（各载体均被评审
    // 否决：chrome.storage 爆配额 / IndexedDB 占浏览器配额且清浏览器静默丢失 / 后端
    // 表与方案 A 防 SQLite 膨胀的立项矛盾）——重开面板后无 full，点击诚实提示重新
    // 截图核对，不展示糊图。旧消息/生成失败（无 thumb）兜底为 image-tag 图标。
    const thumbs = (images || []).filter((im) => im && im.thumb);
    if (thumbs.length) {
      const row = document.createElement('div');
      row.className = 'msg-thumbs';
      for (const im of thumbs) {
        const img = document.createElement('img');
        img.className = 'msg-thumb';
        img.src = im.thumb;
        img.alt = '';
        if (im.full) {
          img.addEventListener('click', () => openImageLightbox(im.full));
          // 图工具栏锚点：img 是替换元素装不下子节点，包一层相对定位容器；
          // 无内存原图（no-full）不挂——导出的是原图，没有原图就没有工具栏
          const holder = document.createElement('span');
          holder.className = 'fig-wrap';
          holder.appendChild(img);
          attachFigToolbar(holder, { kind: 'bitmap', dataUrl: im.full });
          row.appendChild(holder);
        } else {
          img.classList.add('no-full');
          img.title = t('lightbox.originalNotKept');
          // 提示挂容器而非 img：img 是替换元素，::after 伪元素不渲染（B1 挂 button 可用，
          // 挂 img 无效——实测点击无反应的原因）；容器级气泡弹在缩略行正上方，语义不变
          img.addEventListener('click', () => showThumbTip(row, t('lightbox.originalNotKept')));
          row.appendChild(img);
        }
      }
      bubble.appendChild(row);
    } else if (images && images.length) {
      const imgTag = document.createElement('span');
      imgTag.className = 'image-tag';
      imgTag.title = images.length > 1 ? `图片 ×${images.length}` : '图片';
      imgTag.innerHTML = ICON_IMAGE;
      bubble.appendChild(imgTag);
    }
    // 与提问框一致的展示：正文按 [[n]] 占位符拆分，标签在原位行内还原为只读 chip
    // （displayQ 由 collectInput 生成，标签位置即 [[n]]；换行由 .bubble 的 pre-wrap 保留）
    const quotes = (context && Array.isArray(context.quotes)) ? context.quotes : [];
    const re = /\[\[(\d+)\]\]/g;
    let last = 0, m;
    while ((m = re.exec(content)) !== null) {
      if (m.index > last) bubble.appendChild(document.createTextNode(content.slice(last, m.index)));
      const q = quotes[Number(m[1]) - 1];
      if (q) {
        bubble.appendChild(createChipEl(q, true));
      } else {
        bubble.appendChild(document.createTextNode(m[0])); // 快照缺失兜底：原样显示占位符
      }
      last = m.index + m[0].length;
    }
    if (last < content.length) bubble.appendChild(document.createTextNode(content.slice(last)));
  } else {
    // 助手正文：切分可视化块渲染（历史重建即终帧；流式态由 conversation.js 传 final:false）
    bubble.appendChild(renderAssistantContent(content, currentSession()?.sessionId, { final: !streaming }));
  }
  if (streaming) {
    const caret = document.createElement('span');
    caret.className = 'stream-caret';
    bubble.appendChild(caret);
  } else {
    // 最终渲染（历史重建/停止重建）才渲染 mermaid 图；流式期间保持代码块
    decorateMermaid(bubble);
  }

  wrap.appendChild(roleEl);
  // 思考过程折叠块:仅历史重建时渲染(流式时由 conversation.js 动态构造,避免重复)
  // 默认折叠,与流式结束时一致;空 reasoning 不渲染,老数据无此字段也兼容
  if (role === 'assistant' && !streaming && reasoning) {
    const details = document.createElement('details');
    details.className = 'reasoning';
    details.open = false;
    const summary = document.createElement('summary');
    summary.textContent = t('process.thinkingDone');
    const body = document.createElement('div');
    body.className = 'reasoning-body';
    body.textContent = reasoning;
    details.appendChild(summary);
    details.appendChild(body);
    wrap.appendChild(details);
  }
  // 主动停止后保留的半截回答尾部追加"已停止"标记(非气泡正文,灰色小字独立行)
  if (role === 'assistant' && !streaming && stopped) {
    const mark = document.createElement('span');
    mark.className = 'stop-mark';
    mark.textContent = t('buttons.stopped');
    bubble.appendChild(mark);
  }
  wrap.appendChild(bubble);
  return wrap;
}

export function scrollToBottom() {
  els.messages.scrollTop = els.messages.scrollHeight;
}

// 是否粘在底部附近（用于智能跟随：用户滚上看历史时不强制拉回）
const SNAP_THRESHOLD = 80;
export function isNearBottom() {
  const el = els.messages;
  return el.scrollHeight - el.scrollTop - el.clientHeight < SNAP_THRESHOLD;
}

// 提问按钮双态:空闲=提问(蓝,依输入决定 disabled);流式中=停止(红,可点触发 abort)
// 实心红避免蓝→描边闪烁突变;停止后 catch 分支统一清理,此处只管按钮外观
export function updateAskBtn() {
  if (getStreaming()) {
    els.askBtn.textContent = t('buttons.stop');
    els.askBtn.classList.add('danger');
    els.askBtn.disabled = false;
    return;
  }
  els.askBtn.textContent = t('buttons.ask');
  els.askBtn.classList.remove('danger');
  // contenteditable 有文本、有划词标签、或附了图片，即视为可提问
  const el = els.questionInput;
  const hasText = (el.textContent && el.textContent.trim().length > 0) || !!el.querySelector('.quote-chip');
  els.askBtn.disabled = !(hasText || composerImages.length > 0);
}

// ---------- History 渲染 ----------
export function renderHistoryList() {
  els.historyList.innerHTML = '';
  // 本地会话 ∪ 远端索引 stub（sessionId 去重，本地优先），按更新时间归并排序
  const merged = [...state.sessions, ...remoteOnlyStubs().map((r) => ({ ...r, remote: true }))]
    .sort((a, b) => (b.updatedAt || 0) - (a.updatedAt || 0));
  if (merged.length === 0) {
    const li = document.createElement('li');
    li.className = 'history-empty';
    li.textContent = t('empty.historyEmpty');
    els.historyList.appendChild(li);
    return;
  }
  for (const s of merged) {
    const li = document.createElement('li');
    if (s.id === state.currentSessionId) li.classList.add('active');

    const title = document.createElement('div');
    title.className = 'h-title';
    title.textContent = s.remote ? (s.title || t('empty.sessionEmpty')) : historyTitle(s);

    const meta = document.createElement('div');
    meta.className = 'h-meta';
    meta.textContent = `${s.remote ? s.msgCount : s.messages.length} 条消息 · ${formatTime(s.updatedAt || s.createdAt)}`;

    li.appendChild(title);
    li.appendChild(meta);
    li.addEventListener('click', async () => {
      if (getStreaming()) return;
      if (s.remote) {
        // 远端懒加载：详情拉取注入本地缓存后按本地会话打开；失败 toast 保持列表态
        li.classList.add('loading');
        const loaded = await loadRemoteSession(s.sessionId);
        li.classList.remove('loading');
        if (!loaded) {
          showToast({ text: t('ui.remoteLoadFailed'), variant: 'warn' });
          return;
        }
        state.currentSessionId = loaded.id;
      } else {
        state.currentSessionId = s.id;
      }
      // 切换即落盘：面板重开时 loadState 依据此指针恢复"上次会话"，不落盘会恢复到更早的会话
      saveState();
      renderComposerChips();
      renderMessages();
      renderHistoryList();
    });
    els.historyList.appendChild(li);
  }
}

function historyTitle(s) {
  // 历史列表展示用：剥掉 [[n]] 引用占位符，只显示提问描述
  // （消息 content 本身仍保留占位符供后端关联与气泡还原，此处仅影响标题展示）
  const firstUser = s.messages.find((m) => m.role === 'user');
  if (firstUser) {
    const plain = firstUser.content.replace(/\[\[\d+\]\]/g, '').trim();
    if (plain) return plain.slice(0, 24);
  }
  // 首条消息只有引用 chip 没文字（纯划词提问）时，回退到第一条划词文本
  if (Array.isArray(s.quotes) && s.quotes.length) return s.quotes[0].text.slice(0, 24);
  return t('empty.sessionEmpty');
}

function formatTime(ts) {
  const d = new Date(ts);
  const pad = (n) => String(n).padStart(2, '0');
  return `${d.getMonth() + 1}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

// ---------- Toast ----------
let toastTimer = null;
export function showToast({ text, variant = 'info', persist = false, actions = [] }) {
  const el = $('toast');
  el.className = 'toast' + (variant === 'warn' ? ' variant-warn' : '');
  el.innerHTML = '';
  el.appendChild(document.createTextNode(text));
  if (actions.length) {
    const actionsEl = document.createElement('div');
    actionsEl.className = 'toast-actions';
    for (const a of actions) {
      const btn = document.createElement('button');
      btn.className = 'toast-btn' + (a.primary ? ' primary' : '');
      btn.textContent = a.label;
      btn.addEventListener('click', a.onClick);
      actionsEl.appendChild(btn);
    }
    el.appendChild(actionsEl);
  }
  el.hidden = false;
  if (toastTimer) { clearTimeout(toastTimer); toastTimer = null; }
  if (!persist) {
    toastTimer = setTimeout(hideToast, 4000);
  }
}

export function hideToast() {
  const el = $('toast');
  if (el) el.hidden = true;
  if (toastTimer) { clearTimeout(toastTimer); toastTimer = null; }
}
