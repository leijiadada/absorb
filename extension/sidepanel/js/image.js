// Absorb 阅读助手 - 图片交互层
// 职责：来源菜单、区域/元素/视口截图、上传、粘贴、附件 chip 渲染与序列化。
// 不涉及后端：图片仅随 /api/ask 的 payload.images 发送，不写入 chrome.storage。
import { els, placeCaretAtEnd, showToast, updateAskBtn } from './render.js';
import { t } from './i18n.js';
import { composerImages, hasVisionModel } from './state.js';

// ---------- 图标（Lucide 风格内联 SVG，跟随 currentColor，禁止 emoji） ----------
const ICON = {
  region: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M3 7V5a2 2 0 0 1 2-2h2M17 3h2a2 2 0 0 1 2 2v2M21 17v2a2 2 0 0 1-2 2h-2M7 21H5a2 2 0 0 1-2-2v-2"/></svg>',
  element: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="3" width="18" height="18" rx="2"/><path d="M3 9h18M9 21V9"/></svg>',
  viewport: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="2" y="3" width="20" height="14" rx="2"/><path d="M8 21h8M12 17v4"/></svg>',
  upload: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4M17 8l-5-5-5 5M12 3v12"/></svg>'
};

// ---------- 注入页面的裁切层（自包含，无外部依赖，供 chrome.scripting func 注入） ----------
// 返回 Promise<rect|null>：rect = { x,y,w,h, vw, vh }（视口 CSS 像素 + 视口宽高，供侧栏换算像素比）；
// null = 用户取消。Shadow DOM 隔离样式，z-index 拉满，AbortController 统一清理。
export function runCropLayer(mode) {
  // 注意：本函数经 chrome.scripting.executeScript({func}) 序列化注入页面执行，
  // 闭包会丢失（不能引用模块作用域变量），图标必须自包含内联在此处。
  // 新截图开始：若已有进行中的截图，先取消其覆盖层（让其 resolve(null) 并移除自身）
  if (window.__absorbCropCancel) { try { window.__absorbCropCancel(); } catch (_) {} }
  const CHECK_SVG = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><polyline points="20 6 9 17 4 12"/></svg>';
  const X_SVG = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M18 6 6 18M6 6l12 12"/></svg>';
  return new Promise((resolve) => {
    const host = document.createElement('div');
    host.id = 'absorb-crop-root';
    const shadow = host.attachShadow({ mode: 'closed' });
    const style = document.createElement('style');
    style.textContent = `
      :host { all: initial; }
      * { box-sizing: border-box; margin: 0; padding: 0; }
      .overlay { position: fixed; inset: 0; z-index: 2147483647;
        font-family: -apple-system, system-ui, "Segoe UI", sans-serif;
        user-select: none; -webkit-user-select: none; }
      .overlay.region { cursor: crosshair; }
      .overlay.element { cursor: pointer; pointer-events: none; }
      .backdrop { position: fixed; inset: 0; background: rgba(15,17,21,.5); }
      .sel { position: fixed; border: 2px solid #1d1d1f;
        box-shadow: 0 0 0 100vmax rgba(15,17,21,.5); background: rgba(29,29,31,.06); }
      .sel::after { content: attr(data-size); position: fixed; left: 2px; top: 2px; transform: translateY(-100%);
        background: #1d1d1f; color: #fff; font-size: 12px; padding: 1px 6px; border-radius: 4px; white-space: nowrap; }
      .hl { position: fixed; border: 2px solid #1d1d1f; pointer-events: none;
        box-shadow: 0 0 0 100vmax rgba(15,17,21,.5); background: rgba(29,29,31,.06); }
      .bar { position: fixed; display: flex; gap: 8px;
        background: #fff; border: 1px solid #d2d2d7; border-radius: 8px; padding: 6px 8px; box-shadow: 0 4px 16px rgba(0,0,0,.18); }
      .bar button { font: 13px/1 -apple-system, system-ui, sans-serif; border: 1px solid #d2d2d7; background: #fff;
        color: #1d1d1f; border-radius: 6px; padding: 5px 12px; cursor: pointer; }
      .bar button.primary { background: #1d1d1f; border-color: #1d1d1f; color: #fff; }
      .bar button.icon { padding: 5px; display: inline-flex; align-items: center; justify-content: center; }
      .bar button.icon svg { width: 16px; height: 16px; display: block; }
      .hint { position: fixed; bottom: 12px; left: 50%; transform: translateX(-50%);
        background: rgba(15,17,21,.82); color: #fff; font: 12px/1 -apple-system, system-ui, sans-serif;
        padding: 6px 12px; border-radius: 999px; }
      @media (prefers-reduced-motion: reduce) { * { animation: none !important; transition: none !important; } }
    `;
    const overlay = document.createElement('div');
    overlay.className = 'overlay ' + (mode === 'element' ? 'element' : 'region');
    overlay.appendChild(document.createElement('div')).className = 'backdrop';
    const hint = document.createElement('div');
    hint.className = 'hint';
    hint.textContent = (mode === 'element' ? '移动高亮元素，点击选取；Esc 或右键取消' : '拖拽选择区域，Esc 或右键取消');
    overlay.appendChild(hint);
    shadow.appendChild(style);
    shadow.appendChild(overlay);
    document.body.appendChild(host);

    const controller = new AbortController();
    const cleanup = () => { controller.abort(); host.remove(); window.__absorbCropCancel = null; };
    const finish = (rect) => { cleanup(); resolve(rect); };
    window.__absorbCropCancel = () => finish(null); // 注册全局取消钩子，供侧栏/新截图取消本实例
    document.addEventListener('keydown', (e) => {
      if (e.key === 'Escape') finish(null);
      else if (e.key === 'Enter' && mode === 'region' && sel) confirm_();
    }, { signal: controller.signal });
    // 右键取消截图（并抑制页面右键菜单）
    document.addEventListener('contextmenu', (e) => { e.preventDefault(); finish(null); }, { signal: controller.signal });

    let sel = null;

    if (mode === 'element') {
      let hl = null;
      // 裁切层 pointer-events:none，事件落到原页面；监听挂 document（capture 阻断页面选中/聚焦）
      const onMove = (e) => {
        const el = document.elementFromPoint(e.clientX, e.clientY);
        if (!el || el === document.body || el === document.documentElement) { if (hl) { hl.remove(); hl = null; } return; }
        const r = el.getBoundingClientRect();
        if (!hl) { hl = document.createElement('div'); hl.className = 'hl'; overlay.appendChild(hl); }
        hl.style.left = r.left + 'px'; hl.style.top = r.top + 'px';
        hl.style.width = r.width + 'px'; hl.style.height = r.height + 'px';
      };
      const onDown = (e) => { e.preventDefault(); }; // 阻止页面开始文本选中/聚焦
      const onClick = (e) => {
        e.preventDefault(); e.stopPropagation();
        const el = document.elementFromPoint(e.clientX, e.clientY);
        if (!el || el === document.body || el === document.documentElement) return finish(null);
        const r = el.getBoundingClientRect();
        finish({ x: r.left, y: r.top, w: r.width, h: r.height, vw: window.innerWidth, vh: window.innerHeight });
      };
      document.addEventListener('mousemove', onMove, { signal: controller.signal });
      document.addEventListener('mousedown', onDown, { signal: controller.signal, capture: true });
      document.addEventListener('click', onClick, { signal: controller.signal, capture: true });
    } else {
      let startX = 0, startY = 0, drawing = false, bar = null;
      const moveTo = (cx, cy) => {
        const x = Math.min(startX, cx), y = Math.min(startY, cy);
        const w = Math.abs(cx - startX), h = Math.abs(cy - startY);
        sel.style.left = x + 'px'; sel.style.top = y + 'px';
        sel.style.width = w + 'px'; sel.style.height = h + 'px';
        sel.dataset.size = Math.round(w) + '×' + Math.round(h);
      };
      const confirm_ = () => {
        if (!sel) return finish(null);
        const r = sel.getBoundingClientRect();
        finish({ x: r.left, y: r.top, w: r.width, h: r.height, vw: window.innerWidth, vh: window.innerHeight });
      };
      // 操作栏横向右对齐选区右边缘（不超出截图右边）；纵向优先放到选区外侧（鼠标那一侧），避免遮盖截图
      const positionBar = (mx, my) => {
        if (!sel || !bar) return;
        const r = sel.getBoundingClientRect();
        const bw = bar.offsetWidth, bh = bar.offsetHeight;
        const vw = window.innerWidth, vh = window.innerHeight, gap = 8, pad = 8;
        let left = r.right - bw; // 右边缘对齐选区右边
        left = Math.min(Math.max(pad, left), Math.max(pad, vw - bw - pad)); // 夹在视口内
        const below = r.bottom + gap, above = r.top - bh - gap;
        let top = (my >= r.top + r.height / 2) ? below : above; // 鼠标在下半放下方，否则放上方
        if (top < pad || top + bh > vh - pad) top = (top === below) ? above : below; // 该侧不够 → 翻到另一侧
        if (top < pad || top + bh > vh - pad) { // 两侧都放不下 → 退回鼠标附近并夹在视口内
          top = (my + gap + bh > vh - pad) ? my - bh - gap : my + gap;
          top = Math.min(Math.max(pad, top), Math.max(pad, vh - bh - pad));
        }
        bar.style.left = left + 'px';
        bar.style.top = top + 'px';
      };
      const onDown = (e) => {
        if (e.button !== 0) return; // 仅左键起框，右键用于取消
        if (e.target.closest('.bar')) return;
        // 阻止浏览器把拖拽当作文本选择（否则会弹"搜索文本"气泡）
        e.preventDefault();
        const s = window.getSelection();
        if (s) s.removeAllRanges();
        // 重新拖拽前清掉上一次的选区与操作条，避免叠加出多个选框
        if (sel) { sel.remove(); sel = null; }
        if (bar) { bar.remove(); bar = null; }
        drawing = true; startX = e.clientX; startY = e.clientY;
        sel = document.createElement('div'); sel.className = 'sel'; overlay.appendChild(sel);
        moveTo(e.clientX, e.clientY);
      };
      const onMove = (e) => { if (drawing) moveTo(e.clientX, e.clientY); };
      const onUp = (e) => {
        if (!drawing) return;
        drawing = false;
        if (sel && (sel.offsetWidth < 8 || sel.offsetHeight < 8)) { sel.remove(); sel = null; }
        if (sel && !bar) {
          bar = document.createElement('div'); bar.className = 'bar';
          const redo = document.createElement('button'); redo.textContent = '重选'; redo.title = '重选';
          const ok = document.createElement('button'); ok.className = 'icon primary'; ok.innerHTML = CHECK_SVG;
          ok.title = '确认'; ok.setAttribute('aria-label', '确认');
          ok.addEventListener('click', confirm_, { signal: controller.signal });
          const cancel = document.createElement('button'); cancel.className = 'icon'; cancel.innerHTML = X_SVG;
          cancel.title = '取消'; cancel.setAttribute('aria-label', '取消');
          cancel.addEventListener('click', () => finish(null), { signal: controller.signal });
          redo.addEventListener('click', () => { if (sel) { sel.remove(); sel = null; } bar.remove(); bar = null; }, { signal: controller.signal });
          bar.append(redo, ok, cancel); overlay.appendChild(bar);
          positionBar(e.clientX, e.clientY); // 操作栏贴着鼠标释放点展示
        }
      };
      overlay.addEventListener('mousedown', onDown, { signal: controller.signal });
      overlay.addEventListener('mousemove', onMove, { signal: controller.signal });
      window.addEventListener('mouseup', onUp, { signal: controller.signal });
    }
  });
}

// ---------- 图片工具 ----------
function loadImage(url) {
  return new Promise((resolve, reject) => {
    const img = new Image();
    img.onload = () => resolve(img);
    img.onerror = reject;
    img.src = url;
  });
}

// 按视口 rect 从整屏截图裁出区域（rect 视口 px，换算到截图像素比）
function cropImage(full, rect) {
  return new Promise((resolve, reject) => {
    const scale = full.naturalWidth / (rect.vw || window.innerWidth);
    const sx = rect.x * scale, sy = rect.y * scale, sw = rect.w * scale, sh = rect.h * scale;
    const c = document.createElement('canvas');
    c.width = Math.max(1, Math.round(sw));
    c.height = Math.max(1, Math.round(sh));
    try {
      c.getContext('2d').drawImage(full, sx, sy, sw, sh, 0, 0, c.width, c.height);
    } catch (e) { return reject(e); }
    resolve(c.toDataURL('image/png'));
  });
}

// 按来源分流的压缩预设（方案 C）：区域/元素截图（共用同一条 cropImage 路径）文字
// 信息密集，质量优先 0.9；视口截图布局参考为主，0.8 控体积；上传尊重原图，仅超宽
// 缩到 1600。小图不放大由 compressImage 内部保证（w > maxW 才缩）。
// 导出供 vitest 防漂移断言（frontend/tests/extension/Image.spec.ts）。
export const COMPRESS_PRESET = {
  crop:     { maxW: 1280, quality: 0.9 },
  viewport: { maxW: 1280, quality: 0.8 },
  upload:   { maxW: 1600, quality: 0.85 },
};

// 等比缩到 maxW 以内，转 JPEG 控制体积
function compressImage(dataUrl, maxW, quality) {
  return new Promise((resolve, reject) => {
    const img = new Image();
    img.onload = () => {
      let w = img.naturalWidth, h = img.naturalHeight;
      if (w > maxW) { const r = maxW / w; w = maxW; h = Math.round(h * r); }
      const c = document.createElement('canvas');
      c.width = w; c.height = h;
      c.getContext('2d').drawImage(img, 0, 0, w, h);
      resolve({ dataUrl: c.toDataURL('image/jpeg', quality), w: c.width, h: c.height, mime: 'image/jpeg' });
    };
    img.onerror = reject;
    img.src = dataUrl;
  });
}

function fileToDataUrl(file) {
  return new Promise((resolve, reject) => {
    const fr = new FileReader();
    fr.onload = () => resolve(fr.result);
    fr.onerror = reject;
    fr.readAsDataURL(file);
  });
}

function genId() { return 'img-' + Date.now().toString(36) + Math.random().toString(36).slice(2, 6); }

// ---------- 图片大图预览（点击缩略图弹出，单例，Esc / 点空白 / ✕ 关闭） ----------
let _imgPreview = null;
function openImagePreview(dataUrl, w, h) {
  if (_imgPreview) _imgPreview.remove();
  const root = document.createElement('div');
  root.style.cssText = 'position:fixed;inset:0;z-index:100000;background:rgba(15,17,21,.82);' +
    'display:flex;align-items:center;justify-content:center;padding:24px;' +
    'font-family:-apple-system,system-ui,"Segoe UI",sans-serif;';
  const img = document.createElement('img');
  img.src = dataUrl;
  img.alt = '';
  img.style.cssText = 'max-width:90%;max-height:90%;border-radius:8px;background:#fff;' +
    'box-shadow:0 8px 40px rgba(0,0,0,.5);';
  const closeBtn = document.createElement('button');
  closeBtn.type = 'button';
  closeBtn.textContent = '✕';
  closeBtn.title = t('lightbox.close') || '关闭';
  closeBtn.style.cssText = 'position:absolute;top:16px;right:16px;width:36px;height:36px;' +
    'border:none;border-radius:50%;background:#fff;color:#1d1d1f;font-size:18px;line-height:1;' +
    'cursor:pointer;box-shadow:0 2px 8px rgba(0,0,0,.3);';
  if (w && h) closeBtn.title += ` (${w}×${h})`;
  root.append(img, closeBtn);
  const close_ = () => { document.removeEventListener('keydown', onKey); root.remove(); _imgPreview = null; };
  const onKey = (e) => { if (e.key === 'Escape') close_(); };
  root.addEventListener('click', (e) => { if (e.target === root || e.target === closeBtn) close_(); });
  document.addEventListener('keydown', onKey);
  document.body.appendChild(root);
  _imgPreview = root;
}

// ---------- 附件渲染 ----------
export function renderAttachments() {
  const box = els.attachments;
  box.innerHTML = '';
  if (composerImages.length === 0) { box.hidden = true; return; }
  box.hidden = false;
  for (const img of composerImages) {
    const chip = document.createElement('span');
    chip.className = 'image-chip';
    chip.dataset.iid = img.iid;

    const thumb = document.createElement('img');
    thumb.className = 'thumb';
    thumb.src = img.dataUrl;
    thumb.alt = '';
    thumb.title = img.w + '×' + img.h;
    thumb.style.cursor = 'pointer';
    thumb.addEventListener('click', () => openImagePreview(img.dataUrl, img.w, img.h));

    const x = document.createElement('button');
    x.className = 'img-x';
    x.type = 'button';
    x.setAttribute('role', 'button');
    x.setAttribute('aria-label', t('ui.imageRemove'));
    x.title = t('ui.imageRemove');
    x.textContent = '×';

    chip.append(thumb, x);
    box.appendChild(chip);
  }
}

async function addImage(img) {
  // 方案 D：同步生成 192px 缩略（compressImage 小图不放大），随消息持久化供历史
  // 气泡预览——原图仍不写 storage（发送后即弃），仅缩略 dataURL 进 chrome.storage
  let thumb;
  try {
    thumb = (await compressImage(img.dataUrl, 192, 0.7)).dataUrl;
  } catch (e) {
    console.error('[Absorb] 缩略图生成失败（历史预览将降级为图标）:', e);
  }
  composerImages.push({ iid: genId(), dataUrl: img.dataUrl, mime: img.mime, w: img.w, h: img.h, thumb });
  renderAttachments();
  updateAskBtn();
  // 截图/上传完成后把焦点归还输入框并置光标到末尾：来源菜单与页面裁切层会夺走焦点，
  // 不归还的话用户按 Enter 触发不到输入框的发送快捷键（与划词 chip 添加后的行为一致）
  placeCaretAtEnd(els.questionInput);
}

function removeImage(iid) {
  const i = composerImages.findIndex((m) => m.iid === iid);
  if (i >= 0) {
    composerImages.splice(i, 1);
    renderAttachments();
    updateAskBtn();
  }
}

function setImageBtnBusy(busy) {
  els.imageBtn.disabled = busy;
  els.imageBtn.classList.toggle('busy', busy);
  els.imageBtn.title = busy ? t('ui.imageCompressing') : t('ui.imageTip');
}

// ---------- 截图互斥 ----------
// 进行中的截图状态：新截图开始时先取消旧的，避免多个覆盖层叠加 / 残留遮罩被拍进新图。
// 页面侧的取消钩子 window.__absorbCropCancel 由注入的 runCropLayer 注册（同 frame 的 isolated world 共享）。
let _activeCapture = null; // { tabId, token } — 当前进行中的截图
let _captureSeq = 0;       // 递增 token：旧任务凭此丢弃过期结果、不复位新任务状态

// 取消正在进行的截图：先失效 token，再向对应标签页注入取消钩子（移除旧覆盖层并 resolve(null)）
async function cancelActiveCapture() {
  const a = _activeCapture;
  _activeCapture = null;
  if (!a) return;
  try {
    await chrome.scripting.executeScript({
      target: { tabId: a.tabId },
      func: () => { if (window.__absorbCropCancel) window.__absorbCropCancel(); }
    });
  } catch (_) { /* tab 已关闭等，忽略 */ }
}

// ---------- 截图采集 ----------
async function regionCapture(mode) {
  if (!hasVisionModel()) {
    showToast({ text: t('errors.screenshotFailed'), variant: 'warn' });
    return;
  }
  // 新截图开始：先取消上一次仍在进行的截图。
  // 必须在 captureVisibleTab 之前执行，否则残留遮罩会被拍进新图。
  await cancelActiveCapture();
  const myToken = ++_captureSeq;
  try {
    const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
    if (!tab) throw new Error('no tab');
    // 视口截图：直接取整屏，不注入裁切层，也无需记录 active
    if (mode === 'viewport') {
      const url = await chrome.tabs.captureVisibleTab(tab.windowId, { format: 'png' });
      setImageBtnBusy(true);
      const { maxW, quality } = COMPRESS_PRESET.viewport;
      const comp = await compressImage(url, maxW, quality);
      await addImage(comp);
      return;
    }
    // 区域/元素：标记进行中（供后续互斥取消），再整屏截图（裁切层尚未注入，不会出现在截图中），最后注入裁切层取 rect
    _activeCapture = { tabId: tab.id, token: myToken };
    const url = await chrome.tabs.captureVisibleTab(tab.windowId, { format: 'png' });
    const results = await chrome.scripting.executeScript({
      target: { tabId: tab.id },
      func: runCropLayer,
      args: [mode]
    });
    const rect = results && results[0] && results[0].result;
    if (!rect) return; // 用户取消或被新的截图取代
    if (myToken !== _captureSeq) return; // 已被新的截图取代，丢弃本次结果
    setImageBtnBusy(true);
    const full = await loadImage(url);
    const cropped = await cropImage(full, rect);
    const { maxW, quality } = COMPRESS_PRESET.crop;
    const comp = await compressImage(cropped, maxW, quality);
    await addImage(comp);
  } catch (e) {
    console.error('[Absorb] 截图失败:', e);
    const unsupported = e && (e.message || '').toLowerCase().includes('platform');
    showToast({ text: unsupported ? t('errors.screenshotUnsupported') : t('errors.screenshotFailed'), variant: 'warn' });
  } finally {
    // 仅当前任务仍是“有效任务”时才复位状态；已被取代的旧任务不得复位新任务
    if (myToken === _captureSeq) {
      if (_activeCapture && _activeCapture.token === myToken) _activeCapture = null;
      setImageBtnBusy(false);
    }
  }
}

async function handleUpload(file) {
  if (!file || !file.type || !file.type.startsWith('image/')) return;
  setImageBtnBusy(true);
  try {
    const dataUrl = await fileToDataUrl(file);
    const { maxW, quality } = COMPRESS_PRESET.upload;
    const comp = await compressImage(dataUrl, maxW, quality);
    await addImage(comp);
  } catch (e) {
    console.error('[Absorb] 上传图片失败:', e);
    showToast({ text: t('errors.screenshotFailed'), variant: 'warn' });
  } finally {
    setImageBtnBusy(false);
  }
}

// ---------- 来源菜单 ----------
function buildMenu() {
  const items = [
    { id: 'region', label: t('ui.imageRegion'), icon: ICON.region, run: () => regionCapture('region') },
    { id: 'element', label: t('ui.imageElement'), icon: ICON.element, run: () => regionCapture('element') },
    { id: 'viewport', label: t('ui.imageViewport'), icon: ICON.viewport, run: () => regionCapture('viewport') },
    { id: 'upload', label: t('ui.imageUpload'), icon: ICON.upload, run: async () => { await cancelActiveCapture(); els.imageFile.click(); } }
  ];
  const menu = document.createElement('div');
  menu.className = 'img-menu';
  menu.setAttribute('role', 'menu');
  menu.setAttribute('aria-label', t('ui.imageMenuTitle'));
  for (const it of items) {
    const el = document.createElement('button');
    el.type = 'button';
    el.className = 'img-menu-item';
    el.setAttribute('role', 'menuitem');
    el.dataset.id = it.id;
    el.innerHTML = `<span class="ico">${it.icon}</span><span class="lbl">${it.label}</span>`;
    el.addEventListener('click', () => { closeMenu(); it.run(); });
    menu.appendChild(el);
  }
  return menu;
}

let _menuEl = null;
function openMenu() {
  if (_menuEl) return;
  const menu = buildMenu();
  document.body.appendChild(menu);
  const r = els.imageBtn.getBoundingClientRect();
  const mw = menu.offsetWidth;
  const mh = menu.offsetHeight;
  const vh = window.innerHeight;
  const vw = window.innerWidth;
  // 图片按钮位于面板底部工具条：默认向上展开，避免被面板下边缘裁剪（看不到内容）
  const openUp = (vh - r.bottom) < (mh + 8);
  const top = openUp ? Math.max(8, r.top - mh - 6) : (r.bottom + 6);
  // 水平右对齐到按钮，并夹在视口内
  const left = Math.min(Math.max(8, r.right - mw), vw - mw - 8);
  menu.style.top = top + 'px';
  menu.style.left = left + 'px';
  _menuEl = menu;
  els.imageBtn.setAttribute('aria-expanded', 'true');
  // 键盘：方向键移动焦点
  const opts = [...menu.querySelectorAll('.img-menu-item')];
  opts[0].focus();
  const onKey = (e) => {
    const idx = opts.indexOf(document.activeElement);
    if (e.key === 'Escape') { closeMenu(); e.preventDefault(); }
    else if (e.key === 'ArrowDown') { e.preventDefault(); opts[(idx + 1) % opts.length].focus(); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); opts[(idx - 1 + opts.length) % opts.length].focus(); }
  };
  menu.addEventListener('keydown', onKey);
  // 点击外部关闭
  setTimeout(() => document.addEventListener('click', onDocClick, true), 0);
}
function closeMenu() {
  if (!_menuEl) return;
  _menuEl.remove();
  _menuEl = null;
  els.imageBtn.setAttribute('aria-expanded', 'false');
  document.removeEventListener('click', onDocClick, true);
}
function onDocClick(e) {
  if (_menuEl && !_menuEl.contains(e.target) && e.target !== els.imageBtn) closeMenu();
}

// ---------- 初始化 ----------
// 视觉软闸：按 hasVisionModel() 重算图片按钮可用态。可多次调用——启动时按
// settings 缓存初判，/api/capabilities 下发后刷新（见 main.js init 与 state.refreshCapabilities）。
// 禁用态提示走 CSS 气泡（data-tip，样式见 sidepanel.css）：原生 title 的气泡位置由
// 浏览器决定（底部工具栏会弹到按钮下方），且无法承载定位；禁用态须移除 title 避免
// 原生气泡与 CSS 气泡叠加。恢复可用时还原正常态 title（"插入图片"）。
export function setImageGate(enabled) {
  els.imageBtn.disabled = !enabled;
  if (enabled) {
    delete els.imageBtn.dataset.tip;
    els.imageBtn.title = t('ui.imageTip');
  } else {
    els.imageBtn.removeAttribute('title');
    els.imageBtn.dataset.tip = t('ui.imageDisabledTip');
  }
}

export function initImageUI() {
  setImageGate(hasVisionModel());

  els.imageBtn.addEventListener('click', (e) => {
    e.stopPropagation();
    if (els.imageBtn.disabled) return;
    if (_menuEl) closeMenu(); else openMenu();
  });

  els.imageFile.addEventListener('change', () => {
    const f = els.imageFile.files && els.imageFile.files[0];
    els.imageFile.value = '';
    if (f) handleUpload(f);
  });

  // 附件删除（事件委托）
  els.attachments.addEventListener('click', (e) => {
    const x = e.target.closest('.img-x');
    if (x) removeImage(x.closest('.image-chip').dataset.iid);
  });

  // 全局粘贴图片（capture 阶段优先于输入框的纯文本粘贴处理）
  document.addEventListener('paste', async (e) => {
    const items = e.clipboardData && e.clipboardData.items;
    if (!items) return;
    for (const it of items) {
      if (it.type && it.type.startsWith('image/')) {
        const file = it.getAsFile();
        if (file) {
          e.preventDefault();
          e.stopPropagation();
          await cancelActiveCapture(); // 粘贴图片前先取消进行中的截图
          handleUpload(file);
        }
        return;
      }
    }
  }, true);

  renderAttachments();
}
