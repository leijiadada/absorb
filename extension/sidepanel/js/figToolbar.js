// Absorb 阅读助手 - 图卡片工具栏（对齐 Trae 图片三键：复制图片 / 下载 PNG / 新窗口打开）
// 职责：工具栏渲染 + 导出动作。两种图来源统一处理：
//   svg    → mermaid 渲染结果（strict 模式输出纯形状 SVG，无 foreignObject/外链资源，
//            canvas 不会被污染，可安全栅格化）
//   bitmap → 用户消息原图 dataUrl（内存态）
// 只 import i18n，不依赖 render.js（toast 抛错交由 main.js 委托层），避免环引用。
// 来源登记存 WeakMap：流式重渲反复重建 DOM，容器销毁即自动释放，无需手动清理。

import { t } from './i18n.js';

const ICON_COPY = '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M9 9h10a1 1 0 0 1 1 1v10a1 1 0 0 1-1 1H9a1 1 0 0 1-1-1V10a1 1 0 0 1 1-1Z"/><path d="M5 15H4a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1h10a1 1 0 0 1 1 1v1"/></svg>';
const ICON_DOWNLOAD = '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="7 10 12 15 17 10"/><line x1="12" y1="15" x2="12" y2="3"/></svg>';
const ICON_OPEN = '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6"/><polyline points="15 3 21 3 21 9"/><line x1="10" y1="14" x2="21" y2="3"/></svg>';

const sources = new WeakMap();

// attachFigToolbar 在图容器右上角挂悬浮三键（CSS hover/focus-within 显示）。
// container 需为定位上下文（.mermaid-figure / .fig-wrap 由样式侧保证 position:relative）。
// data-fig-toolbar 标记供 main.js 事件委托反查来源容器。
export function attachFigToolbar(container, source) {
  if (!container || sources.has(container)) return;
  sources.set(container, source);
  container.dataset.figToolbar = '1';
  const bar = document.createElement('div');
  bar.className = 'fig-toolbar';
  // 图标为静态常量、文案走 i18n（自有 key，非用户内容），可安全 innerHTML
  bar.innerHTML =
    `<button class="fig-btn" data-act="copy" type="button" title="${t('fig.copy')}" aria-label="${t('fig.copy')}">${ICON_COPY}</button>` +
    `<button class="fig-btn" data-act="download" type="button" title="${t('fig.download')}" aria-label="${t('fig.download')}">${ICON_DOWNLOAD}</button>` +
    `<button class="fig-btn" data-act="open" type="button" title="${t('fig.open')}" aria-label="${t('fig.open')}">${ICON_OPEN}</button>`;
  container.appendChild(bar);
}

// handleFigAction：main.js 事件委托入口。成功返回 true（复制按钮 morph 反馈用）；
// 失败抛 Error 由调用方 toast（文案按动作区分，本模块不碰 UI 提示）。
export async function handleFigAction(act, container) {
  const src = sources.get(container);
  if (!src) return false;
  if (act === 'copy') {
    await copyFig(src);
    return true;
  }
  if (act === 'download') {
    await downloadFig(src);
    return true;
  }
  if (act === 'open') {
    await openFig(src);
    return true;
  }
  return false;
}

// ---------- PNG 导出 ----------

// loadImage：dataUrl/URI → 解码完成的 Image 元素
function loadImage(src) {
  return new Promise((resolve, reject) => {
    const img = new Image();
    img.onload = () => resolve(img);
    img.onerror = () => reject(new Error('图片解码失败'));
    img.src = src;
  });
}

// canvasToPng：canvas → PNG Blob（toBlob 回调风格转 Promise）
function canvasToPng(canvas) {
  return new Promise((resolve, reject) => {
    canvas.toBlob((b) => (b ? resolve(b) : reject(new Error('PNG 编码失败'))), 'image/png');
  });
}

// svgToPngBlob：内联 SVG → 2x 高清 PNG。底色补白——mermaid default 主题按白底
// 设计，透明导出在深色查看器里会出现透明格。clone 锁定 viewBox 原始尺寸并去除
// mermaid 内联 max-width（避免按面板压缩宽度导出糊图）。
async function svgToPngBlob(svgEl, scale = 2) {
  const vb = svgEl.viewBox && svgEl.viewBox.baseVal;
  const rect = svgEl.getBoundingClientRect();
  const w = (vb && vb.width) || rect.width || 800;
  const h = (vb && vb.height) || rect.height || 600;
  if (!w || !h) throw new Error('SVG 尺寸无效');
  const clone = svgEl.cloneNode(true);
  clone.removeAttribute('style');
  clone.setAttribute('xmlns', 'http://www.w3.org/2000/svg');
  clone.setAttribute('width', w);
  clone.setAttribute('height', h);
  const xml = new XMLSerializer().serializeToString(clone);
  const img = await loadImage('data:image/svg+xml;charset=utf-8,' + encodeURIComponent(xml));
  const canvas = document.createElement('canvas');
  canvas.width = Math.round(w * scale);
  canvas.height = Math.round(h * scale);
  const ctx = canvas.getContext('2d');
  ctx.fillStyle = '#ffffff';
  ctx.fillRect(0, 0, canvas.width, canvas.height);
  ctx.drawImage(img, 0, 0, canvas.width, canvas.height);
  return canvasToPng(canvas);
}

// figPngBlob：两种来源统一产出 PNG Blob（位图按原始分辨率栅格化）
async function figPngBlob(src) {
  if (src.kind === 'svg') return svgToPngBlob(src.svg);
  const img = await loadImage(src.dataUrl);
  const canvas = document.createElement('canvas');
  canvas.width = img.naturalWidth;
  canvas.height = img.naturalHeight;
  canvas.getContext('2d').drawImage(img, 0, 0);
  return canvasToPng(canvas);
}

// ---------- 三个动作 ----------

// copyFig：PNG 写剪贴板（扩展页面 secure context + 用户手势，manifest 已声明
// clipboardWrite；ClipboardItem Chrome 120+ 原生支持，与 manifest minVersion 一致）
async function copyFig(src) {
  const blob = await figPngBlob(src);
  await navigator.clipboard.write([new ClipboardItem({ 'image/png': blob })]);
}

// downloadFig：a[download] 触发浏览器下载（文件名时间戳防重名）
async function downloadFig(src) {
  const a = document.createElement('a');
  if (src.kind === 'bitmap') {
    a.href = src.dataUrl;
  } else {
    const blob = await figPngBlob(src);
    a.href = URL.createObjectURL(blob);
    setTimeout(() => URL.revokeObjectURL(a.href), 5000); // 下载启动后释放
  }
  a.download = `absorb-${src.kind === 'svg' ? 'diagram' : 'image'}-${Date.now()}.png`;
  a.click();
}

// openFig：新标签打开。data: URL 顶层导航被 Chrome 禁止，位图先转 PNG blob；
// SVG 以矢量原样打开（可继续缩放查看细节）。blob URL 由本扩展页面创建，tabs.create 可导航。
async function openFig(src) {
  if (src.kind === 'bitmap') {
    const blob = await figPngBlob(src);
    openBlobTab(blob, 'image/png');
    return;
  }
  const clone = src.svg.cloneNode(true);
  clone.removeAttribute('style');
  clone.setAttribute('xmlns', 'http://www.w3.org/2000/svg');
  const xml = new XMLSerializer().serializeToString(clone);
  openBlobTab(new Blob([xml], { type: 'image/svg+xml' }), 'image/svg+xml');
}

// openBlobTab：blob URL 开新标签。延迟释放——标签加载时机不可知，过早 revoke 会白屏；
// 60s 足够加载完成且内存占用可忽略。
function openBlobTab(blob) {
  const url = URL.createObjectURL(blob);
  chrome.tabs.create({ url });
  setTimeout(() => URL.revokeObjectURL(url), 60000);
}
