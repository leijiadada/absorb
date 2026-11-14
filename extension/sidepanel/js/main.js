// Absorb 阅读助手 - 入口
// 职责：init 流程 + 全部事件绑定（输入框/按钮/引用关闭/历史抽屉/重连条/划词监听）
import {
  state, getStreaming, loadState, loadSettings, loadDraft,
  saveState, currentSession, consumePendingQuotes, refreshCapabilities
} from './state.js';
import {
  els, renderComposerChips, appendNewQuoteChip, renderMessages, renderHistoryList,
  updateAskBtn, updatePlaceholder, placeCaretAtEnd, restoreDraft, syncDraft,
  ICON_COPY, ICON_CHECK, openLightbox, showToast
} from './render.js';
import { t, applyDomI18n } from './i18n.js';
import { getBridgePort } from './net.js';
import { refreshRemoteIndex } from './sync.js';
import { submitQuestion, stopStreaming, consumeAndRenderChips } from './conversation.js';
import { initImageUI, setImageGate } from './image.js';
import { handleFigAction } from './figToolbar.js';
import { initDesktopGuide } from './desktop-guide.js';
import { initDocCard } from './doccard.js';

// ---------- 事件绑定 ----------
// contenteditable 输入变化 → 更新占位与按钮可用态 + 同步草稿（防抖落 storage）
els.questionInput.addEventListener('input', () => {
  updatePlaceholder();
  updateAskBtn();
  syncDraft();
});
// 粘贴净化:强制按纯文本插入。剪贴板里的 URL 常带富文本格式(text/html 是 <a href>标题</a>)，
// 默认粘贴会插入链接元素——显示标题文字、URL 藏在 href 里，序列化后丢失导致 agent 拿不到链接
els.questionInput.addEventListener('paste', (e) => {
  e.preventDefault();
  const text = e.clipboardData?.getData('text/plain') || '';
  if (text) document.execCommand('insertText', false, text);
});
els.questionInput.addEventListener('keydown', (e) => {
  if (e.key === 'Enter' && !e.shiftKey) {
    e.preventDefault();
    submitQuestion();
  }
  // Backspace 删除整块标签:当光标紧邻某标签前捕获到删除动作时,由浏览器原生兜底;
  // 此处仅保证删除后可编辑文本稳定,原生已可删 contenteditable=false 块,无需额外处理。
});
// 提问按钮双态:流式中点=停止(abort,框架 ctx 取消停止后端);空闲点=提问
els.askBtn.addEventListener('click', () => {
  if (getStreaming()) stopStreaming();
  else submitQuestion();
});

// 划词标签删除:委托到输入框,点标签内 × 移除整块(同时从当前会话 quotes 移除,避免脏数据)
els.questionInput.addEventListener('click', async (e) => {
  const x = e.target.closest('.chip-x');
  if (!x) return;
  const chip = x.closest('.quote-chip');
  if (!chip) return;
  const qid = chip.dataset.qid;
  chip.remove();
  const s = currentSession();
  if (s && s.quotes) s.quotes = s.quotes.filter((q) => String(q.id) !== qid);
  await saveState();
  updatePlaceholder();
  updateAskBtn();
  syncDraft(); // chip 手动删除后草稿同步（chip 变化不触发 input 事件）
  placeCaretAtEnd(els.questionInput);
});

els.newSession.addEventListener('click', () => {
  if (getStreaming()) return;
  state.currentSessionId = null;
  // 立即落盘：否则关面板后 storage 里仍是旧会话 id，重开面板 loadState 恢复旧会话，
  // 与划词消费竞态（init 误判 hasSel 为空 → 清掉已落框的 chip、划词成孤儿数据）
  saveState();
  // 清空输入框：旧会话的划词 chip 残留在框里时，其 qid 在新会话 quotes 中查不到，
  // 提交后会静默降级成裸 [[n]] 占位符且无参考材料（模型收不到引用文本）
  els.questionInput.textContent = '';
  renderComposerChips();
  renderMessages();
  els.historyDrawer.hidden = true;
  renderHistoryList();
  els.questionInput.focus();
});

els.toggleHistory.addEventListener('click', (e) => {
  e.stopPropagation();
  els.historyDrawer.hidden = !els.historyDrawer.hidden;
  if (!els.historyDrawer.hidden) renderHistoryList();
});
els.closeHistory.addEventListener('click', (e) => {
  e.stopPropagation();
  els.historyDrawer.hidden = true;
});

// 从桌面恢复（手动入口兜底）：强制跳过 TTL 全量拉索引，完成后刷新归并列表。
// 自动恢复在 init 已做首次拉取，此入口覆盖桌面端晚启动/索引中途变更的场景。
els.restoreHistory.addEventListener('click', async () => {
  const btn = els.restoreHistory;
  btn.disabled = true;
  try {
    await refreshRemoteIndex(true);
  } finally {
    btn.disabled = false;
  }
  renderHistoryList();
});

// 点击抽屉外区域（面板其他部分）时关闭历史侧边栏
document.addEventListener('click', (e) => {
  if (els.historyDrawer.hidden) return;
  if (els.historyDrawer.contains(e.target)) return;
  if (els.toggleHistory.contains(e.target)) return;
  els.historyDrawer.hidden = true;
});

// 代码块复制 + 图工具栏 + 图表放大：事件委托到消息容器（流式反复重渲 innerHTML，委托避免重复绑定）
els.messages.addEventListener('click', async (e) => {
  // 图工具栏三键：复制图片 / 下载 PNG / 新窗口打开。分支须在 .mermaid-figure 之前，
  // 否则工具栏点击会冒泡命中图容器触发 lightbox。成功后复制键 morph「✓」反馈
  // （与 code-copy 同形态），失败按动作区分 toast
  const figBtn = e.target.closest('.fig-btn');
  if (figBtn) {
    const host = figBtn.closest('[data-fig-toolbar]');
    const act = figBtn.dataset.act;
    if (host && act) {
      try {
        const ok = await handleFigAction(act, host);
        if (ok && act === 'copy') {
          figBtn.innerHTML = ICON_CHECK;
          figBtn.classList.add('done');
          setTimeout(() => {
            figBtn.innerHTML = ICON_COPY;
            figBtn.classList.remove('done');
          }, 1200);
        }
      } catch (err) {
        console.warn('[Absorb] 图导出失败:', err);
        showToast({
          text: act === 'copy' ? t('fig.copyFailed') : t('fig.exportFailed'),
          variant: 'warn',
        });
      }
    }
    return;
  }
  // mermaid 图：点击弹出全屏查看器（滚轮缩放/拖拽平移）
  const fig = e.target.closest('.mermaid-figure');
  if (fig) {
    const svg = fig.querySelector('svg');
    if (svg) openLightbox(svg);
    return;
  }
  const btn = e.target.closest('.code-copy');
  if (!btn) return;
  const pre = btn.closest('pre');
  const codeEl = pre?.querySelector('code');
  if (!codeEl) return;
  const text = codeEl.textContent || '';
  const copied = await copyText(text);
  if (copied) {
    btn.innerHTML = ICON_CHECK;
    btn.title = t('copy.copied');
    btn.classList.add('done');
    setTimeout(() => {
      btn.innerHTML = ICON_COPY;
      btn.title = t('copy.copy');
      btn.classList.remove('done');
    }, 1200);
  }
});

// 复制到剪贴板：优先 async clipboard API（侧边栏为 secure context），失败回退 execCommand
async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch (err) {
    console.warn('[Absorb] clipboard API 失败，回退 execCommand:', err);
    try {
      const ta = document.createElement('textarea');
      ta.value = text;
      ta.style.position = 'fixed';
      ta.style.opacity = '0';
      document.body.appendChild(ta);
      ta.select();
      const ok = document.execCommand && document.execCommand('copy');
      ta.remove();
      return ok;
    } catch (e) {
      console.error('[Absorb] 复制失败:', e);
      return false;
    }
  }
}

// ---------- 重连条 ----------
// 连接失败时由 conversation.js 显示；点击重测 /health，成功后自动隐藏。
// 桥端口由 OS 动态分配：重连前先经 SW 发现当前端口，未知时按"桌面端未就绪"提示。
els.reconnectBtn.addEventListener('click', async () => {
  const bar = els.reconnectBar;
  const btn = els.reconnectBtn;
  btn.disabled = true;
  bar.classList.remove('error'); // 回到中性"测试中"态（灰灯）
  els.reconnectMsg.textContent = t('process.testing');
  try {
    const port = await getBridgePort();
    if (!port) {
      bar.classList.add('error');
      els.reconnectMsg.textContent = t('errors.desktopNotReady');
    } else {
      const resp = await fetch(`http://localhost:${port}/health`);
      if (resp.ok) {
        // 连上即收：直接隐藏，不做单独的成功展示
        bar.hidden = true;
        els.reconnectMsg.textContent = '';
      } else {
        bar.classList.add('error');
        els.reconnectMsg.textContent = t('errors.testConnError', { status: resp.status });
      }
    }
  } catch (e) {
    bar.classList.add('error');
    els.reconnectMsg.textContent = t('errors.testConnFailed');
  }
  btn.disabled = false;
});

// 面板已打开时，再次右键划词 -> 追加为输入框新标签（流式期间暂存，结束后重试）
// 先等 init 完成（屏障）：init 未完成时消费，会把划词塞进旧会话或被 loadState 快照覆盖；
// 且 init 的 hasSel 判定会因游标被提前推进而误判为"没划词"→ 置空 currentSessionId
// + renderComposerChips 清框，把已落框的 chip 清掉、划词变成孤儿数据
chrome.storage.onChanged.addListener((changes, area) => {
  if (area !== 'local') return;
  if (changes.pendingQuotes && changes.pendingQuotes.newValue && !getStreaming()) {
    waitForInit().then(() => consumeAndRenderChips()).then((fresh) => {
      if (!fresh || fresh.length === 0) return;
      // chip 落入后聚焦输入框、光标定位到末尾（chip 之后）：
      // 面板已持有焦点时划词即可直接打字；焦点在网页时 focus 无效
      // （Chrome 无法将系统焦点转移给 sidepanel），仍需先点一下面板
      els.questionInput.focus();
      placeCaretAtEnd(els.questionInput);
    });
  }
});

// ---------- Init ----------
async function init() {
  // HTML 静态文案按浏览器语言替换（先于一切渲染，防中英混闪）
  applyDomI18n();
  console.log('[Absorb] sidepanel init 开始, isStreaming =', getStreaming());
  await loadSettings();
  await loadState();

  // 划词（若有）并入当前会话；无划词（图标点击）保持 loadState 恢复的上次会话，
  // 首次使用 currentSessionId 本为 null → 自然空态
  const fresh = await consumePendingQuotes();

  // 确保历史抽屉默认收起（图标点击打开时不要盖在面板上）
  els.historyDrawer.hidden = true;

  renderComposerChips();
  // 草稿回填：恢复关面板前未发送的输入（chip 按快照 qid 还原，查不到的占位符丢弃）
  const draft = await loadDraft();
  if (draft) restoreDraft(draft);
  // 关面板期间新划入的词追加在草稿之后（不覆盖旧草稿）。
  // 无草稿时 renderComposerChips 已渲染过 fresh（quotes 已并入会话），按 qid 去重防重复；
  // 有草稿时 restoreDraft 重建会丢弃 fresh 占位符，此处守卫通过、正常追加
  if (Array.isArray(fresh)) fresh.forEach((q) => {
    const exists = els.questionInput.querySelector(`.quote-chip[data-qid="${q.id}"]`);
    if (!exists) appendNewQuoteChip(q);
  });
  renderMessages();
  renderHistoryList();
  updateAskBtn();
  initImageUI();
  // 桌面端未安装引导：异步探测宿主注册状态，未安装时空态区显示下载卡片（不阻塞 init）
  initDesktopGuide();
  // 文档卡（PDF 解读）：识别活动 PDF 标签页渲染三态卡片；建议问题经 ask 落
  // 输入框后走 submitQuestion 全链路（追加而非清空，不吞用户已输入的草稿/chip）
  initDocCard({
    ask: (text) => {
      const elx = els.questionInput;
      elx.appendChild(document.createTextNode(text));
      elx.dispatchEvent(new InputEvent('input', { bubbles: true }));
      submitQuestion();
    }
  });
  // 能力下发刷新软闸：不阻塞 init（桥端口发现可能慢/失败，失败保持 settings 缓存初判）
  refreshCapabilities().then((caps) => {
    if (caps !== null) setImageGate(caps);
  });
  // 自动恢复（设计定稿：面板打开全量拉索引）：远端索引与本地 diff 后归并进历史列表。
  // 不阻塞 init，拉到数据才重渲列表；桌面不可达时静默保持本地模式
  refreshRemoteIndex().then((idx) => {
    if (idx && idx.length) renderHistoryList();
  });
  // placeCaretAtEnd 而非 focus()：focus 在 selection 未设定时会把 caret 放到内容开头，
  // 视觉落在最左侧 chip 前方；显式把光标定位到内容末尾（草稿文字之后）
  placeCaretAtEnd(els.questionInput);
}

// init 完成屏障：onChanged 与 init 共享同一 promise。
// 划词消费必须等 loadState 读完 + init 自身的消费跑完，否则并发交错导致
// hasSel 误判（init 拿到被推进游标后的 null）、已落框 chip 被 init 清空
let _initDone = null;
function waitForInit() {
  if (!_initDone) _initDone = init();
  return _initDone;
}

waitForInit();
