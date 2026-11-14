// 右键菜单"Absorb 解读"：先在用户手势的同步上下文里开面板，有划词再采集。
// 采集经 executeScript 注入页面隔离世界（Defuddle 与采集函数定义见 grab.js），
// 结果进入待取队列，面板打开后自动填充到提问框。

import { appendPendingQuote } from './quote-queue.js';

// 敏感日志开关：上下文摘要含划词文本与页面内容预览，发布版保持静默，排查时临时打开
const DEBUG = false;

export const PANEL_MENU_ID = 'absorb-open-panel';

export function registerMenu() {
  chrome.contextMenus.removeAll();
  chrome.contextMenus.create({
    id: PANEL_MENU_ID,
    title: chrome.i18n.getMessage('menuOpenPanel') || 'Absorb 解读',
    contexts: ['all']
  });
}

export async function handleMenuClick(info, tab, skipPanelOpen = false) {
  if (DEBUG) console.log('[Absorb] contextMenu clicked: menuId =', info.menuItemId, 'tab.id =', tab?.id, 'tab.url =', tab?.url);
  if (info.menuItemId !== PANEL_MENU_ID) return;

  // 开面板：必须在用户手势的同步上下文里调，不能放到 await 之后，
  // 否则报 "sidePanel.open() may only be called in response to a user gesture"
  // PDF 查看器等特殊场景下 tab.id 可能为 -1，回退到 windowId（同步常量，不破坏手势）
  // E2E 传 skipPanelOpen 跳过开面板：测试自开面板页，避免多面板实例竞争消费划词队列
  if (!skipPanelOpen) {
    const openOpts = (tab && typeof tab.id === 'number' && tab.id > 0)
      ? { tabId: tab.id }
      : { windowId: chrome.windows.WINDOW_ID_CURRENT };
    console.log('[Absorb] sidePanel.open 调用, opts =', openOpts);
    chrome.sidePanel.open(openOpts).then(() => {
      console.log('[Absorb] sidePanel.open 成功');
    }).catch((err) => {
      console.error('[Absorb] sidePanel.open 失败:', err);
    });
  }

  // 无划词：只开面板，不采集
  const sel = (info.selectionText || '').trim();
  if (!sel) return;

  // 有划词：抓取选区 + 所在段落上下文 + 整页结构化文本
  let ctx = {
    text: sel,
    paragraph: '',
    pageContext: '',
    url: tab?.url || '',
    pageMd: '',
    isComplete: true
  };
  try {
    const results = await chrome.scripting.executeScript({
      target: { tabId: tab.id, frameIds: [info.frameId] },
      files: ['sidepanel/vendor/defuddle.min.js', 'html-pruner.js', 'pageconvert.js', 'grab.js']
    });
    if (results && results[0] && results[0].result) {
      ctx = results[0].result;
      if (!ctx.url) ctx.url = tab?.url || '';
      // 浏览器内置 PDF 查看器（Chrome/Edge）内划词：grab 所在 frame 的
      // location.href 是查看器扩展页地址（chrome-extension://.../edge_pdf/
      // index.html），真实文档地址在顶层 tab.url。不纠正会污染 scope 归组、
      // 摘要与知识保存的来源归属（grab.js 用 location.href 自报，无法自救）。
      if (tab?.url && /^https?:/i.test(tab.url) && /^(chrome|moz)-extension:/i.test(ctx.url || '')) {
        ctx.url = tab.url;
      }
    }
  } catch (e) {
    // 跨域 iframe 注入失败时回退：仅用选中句（无段落上下文）
    console.warn('[Absorb] 上下文抓取失败，回退纯选中句:', e);
  }

  // 打印抓取到的上下文摘要（pageMd 全文过大，只打印长度与前 60 字符）；含页面内容，仅 DEBUG
  if (DEBUG) console.log('[Absorb] 抓取上下文: text=%d字 paragraph=%d字 pageContext=%d字 pageMd=%d字 isComplete=%s\n  pageMd 预览: %s\n  url: %s',
    (ctx.text || '').length, (ctx.paragraph || '').length, (ctx.pageContext || '').length,
    (ctx.pageMd || '').length, ctx.isComplete,
    (ctx.pageMd || '').slice(0, 60).replace(/\s+/g, ' '),
    ctx.url);
  await appendPendingQuote(ctx);
}
