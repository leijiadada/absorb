// Absorb 阅读助手 - 知识保存面板（M2 吸收闭环）
// 职责：「把这次理解留下来」入口渲染 + 保存面板状态机
// （草稿加载 → 编辑 → 保存中 → 成功/幂等命中/失败可重试）。
// 输入为当轮消息快照（user 消息 context + assistant 消息 requestId），不碰消息渲染层；
// 用户衍生的内容一律 textContent/value 赋值注入，不走模板插值。

import { t } from './i18n.js';
import { fetchKnowledgeDraft, saveKnowledge, saveReadingSummary } from './net.js';
import { els, showToast } from './render.js';

// 单开互斥：同一时刻至多一个面板展开（打开新面板先收起旧面板）。
let openState = null; // { panel, entryBtn, msgNode, ctx, saved, userScrolled }
// 已填内容内存草稿（键 originEventId）：收起重开不丢字；保存成功即清。
const editDrafts = new Map();

// 用户滚动标记（懒挂载一次）：面板打开期间 wheel/touchmove 即视为用户在主动
// 浏览，后续自动滚入全部跳过，不打断阅读。自身的 smooth scrollIntoView 不产生
// wheel/touch 事件，无自触发回路。不能在模块顶层立即挂载——本模块与 render.js
// 是环引用，环上 els 尚未初始化（其余 render.js 导入均只在调用期解引用，故无碍）。
let userScrollHooked = false;
function hookUserScroll() {
  if (userScrollHooked || !els.messages) return;
  userScrollHooked = true;
  const mark = () => { if (openState) openState.userScrolled = true; };
  els.messages.addEventListener('wheel', mark, { passive: true });
  els.messages.addEventListener('touchmove', mark, { passive: true });
}

// revealPanel 条件滚入：面板仍在打开态且用户未手动滚动时滚入视野。
// block:'nearest' 幂等——已完整可见则不动，故展开结束与草稿到达两次
// 调用安全；scroll-margin-bottom 提供底部留缝。
function revealPanel(panel) {
  if (!openState || openState.panel !== panel || openState.userScrolled) return;
  panel.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
}

const ICON_BOOK = '<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M12 5c-1.9-1.4-4.3-2-8-2v15c3.7 0 6.1.6 8 2 1.9-1.4 4.3-2 8-2V3c-3.7 0-6.1.6-8 2z"/><path d="M12 5v15"/></svg>';
const ICON_BOOK_SM = '<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 5c-1.9-1.4-4.3-2-8-2v15c3.7 0 6.1.6 8 2 1.9-1.4 4.3-2 8-2V3c-3.7 0-6.1.6-8 2z"/><path d="M12 5v15"/></svg>';
const ICON_LINK = '<svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M10 13a5 5 0 0 0 7.5.5l3-3a5 5 0 0 0-7-7l-1.7 1.7"/><path d="M14 11a5 5 0 0 0-7.5-.5l-3 3a5 5 0 0 0 7 7l1.7-1.7"/></svg>';
const ICON_CHECK = '<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"><path d="M20 6 9 17l-5-5"/></svg>';
const ICON_SPIN = '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round"><path d="M12 3a9 9 0 1 0 9 9"/></svg>';

// ---------- 上下文组装 ----------

// keepContextFrom 从当轮消息快照组装保存上下文。question 还原 [[n]] 占位为
// 引用摘要（与 net.js 同款 12 字 digest，占位符不能原样发给模型）。
export function keepContextFrom(session, userMsg, assistantMsg) {
  const quotes = (userMsg && userMsg.context && Array.isArray(userMsg.context.quotes))
    ? userMsg.context.quotes.filter((q) => q && q.text)
    : [];
  const page = (userMsg && userMsg.context && userMsg.context.page) || null;
  let question = (userMsg && userMsg.content) || '';
  question = question.replace(/\[\[(\d+)\]\]/g, (_, n) => {
    const q = quotes[Number(n) - 1];
    if (!q) return '';
    const s = String(q.text).replace(/\s+/g, ' ').trim();
    return s.length > 12 ? s.slice(0, 12) + '…' : (s || '引用内容');
  });
  return {
    question,
    answer: (assistantMsg && assistantMsg.content) || '',
    quotes,
    page,
    originSessionId: (session && session.sessionId) || '',
    originEventId: (assistantMsg && assistantMsg.requestId) || ''
  };
}

// ---------- 入口按钮 ----------

// attachKnowledgeEntry 在回答消息节点下方渲染保存入口（仅完成的回答调用：
// 流式成功结束与历史渲染两条路径）。入口按用户任务分化（M2.5 Entry Rules）：
//   有划词 → 吸收主入口（胶囊「把这次理解留下来」，编辑提炼+证据后入库）
//   无划词+有页面 → 摘要主入口（胶囊「保存这次摘要」，回答全文轻存文档锚点）
//                    + 吸收降为文字链接次入口
//   兜底（无页面）→ 吸收主入口（知识仍可入，摘要无处挂锚点）
export function attachKnowledgeEntry(msgNode, ctx) {
  const row = document.createElement('div');
  row.className = 'msg-actions keep-actions';
  const canSummarize = !ctx.quotes.length && !!(ctx.page && ctx.page.url);
  if (canSummarize) {
    row.appendChild(buildSummaryEntry(ctx));
    row.appendChild(buildAbsorbEntry(msgNode, ctx, 'keep-entry-text'));
  } else {
    row.appendChild(buildAbsorbEntry(msgNode, ctx, 'keep-entry'));
  }
  msgNode.appendChild(row);
  return row;
}

// buildAbsorbEntry 吸收入口：点击开面板；已开时再点收起（toggle）。
// cls 控制形态：主入口胶囊 keep-entry / 次入口文字链接 keep-entry-text。
function buildAbsorbEntry(msgNode, ctx, cls) {
  const btn = document.createElement('button');
  btn.type = 'button';
  btn.className = cls;
  // 图标为静态常量、文案走 i18n，均非用户内容，可安全 innerHTML
  btn.innerHTML = ICON_BOOK_SM + '<span>' + t('knowledge.entry') + '</span>';
  btn.addEventListener('click', () => {
    if (openState && openState.entryBtn === btn) {
      closeKeepPanel(false);
      return;
    }
    openKeepPanel(ctx, btn, msgNode);
  });
  return btn;
}

// buildSummaryEntry 摘要主入口：轻操作不弹面板，回答全文直接保存到页面文档
// 锚点（幂等，同回答重复点击无害）。is-loading morph →「✓ 已保存」定格禁用；
// 失败 toast + 恢复可重试。
function buildSummaryEntry(ctx) {
  const btn = document.createElement('button');
  btn.type = 'button';
  btn.className = 'keep-entry keep-entry-summary';
  btn.innerHTML = ICON_BOOK_SM + '<span class="keep-entry-label">' + t('knowledge.entrySummary') + '</span>';
  let saved = false;
  btn.addEventListener('click', async () => {
    if (saved || btn.classList.contains('is-loading')) return;
    btn.classList.add('is-loading');
    btn.disabled = true;
    const labelEl = btn.querySelector('.keep-entry-label');
    labelEl.textContent = t('knowledge.saving');
    try {
      await saveReadingSummary({
        sessionId: ctx.originSessionId,
        requestId: ctx.originEventId,
        url: (ctx.page && ctx.page.url) || '',
        title: (ctx.page && ctx.page.title) || '',
        summaryText: ctx.answer
      });
      saved = true;
      btn.classList.remove('is-loading');
      btn.classList.add('is-done');
      labelEl.textContent = t('knowledge.saved');
      showToast({ text: t('knowledge.summarySaved') });
    } catch (err) {
      btn.classList.remove('is-loading');
      btn.disabled = false;
      labelEl.textContent = t('knowledge.entrySummary');
      showToast({ text: (err && err.message) || t('knowledge.summarySaveFailed'), variant: 'warn' });
    }
  });
  return btn;
}

// ---------- 面板 ----------

function openKeepPanel(ctx, entryBtn, msgNode) {
  hookUserScroll();
  if (openState) closeKeepPanel(false);

  const panel = document.createElement('div');
  panel.className = 'keep-panel';
  panel.innerHTML = [
    '<div class="keep-clip"><section class="keep-card">',
    '<header class="keep-head"><span class="keep-glyph">' + ICON_BOOK + '</span>',
    '<div><h4 class="keep-title" data-kt="title"></h4><p class="keep-subtitle" data-kt="subtitle"></p></div></header>',
    '<div class="keep-edit">',
    '<div class="keep-field" data-kf="titleField"><label class="keep-label"><span data-kt="titleLabel"></span><em data-kt="titleHint"></em></label>',
    '<div class="keep-skel"><i></i></div><input class="keep-input" type="text" data-kf="title"></div>',
    '<div class="keep-field" data-kf="quoteField"><label class="keep-label"><span data-kt="quoteLabel"></span><em data-kt="quoteHint"></em></label>',
    '<div class="keep-quotes" data-kf="quotes"></div><p class="keep-note" data-kf="noQuote" hidden></p></div>',
    '<div class="keep-field" data-kf="summaryField"><label class="keep-label"><span data-kt="summaryLabel"></span><em data-kt="summaryHint"></em></label>',
    '<div class="keep-skel"><i></i><i class="short"></i></div><textarea class="keep-input" rows="3" data-kf="summary"></textarea></div>',
    '<div class="keep-field"><label class="keep-label"><span data-kt="understandingLabel"></span><em data-kt="understandingHint"></em></label>',
    '<textarea class="keep-input" rows="2" data-kf="understanding"></textarea></div>',
    '<p class="keep-note warn" data-kf="degraded" hidden></p>',
    '<p class="keep-source">' + ICON_LINK + '<span data-kt="sourceLabel"></span><a data-kf="sourceLink" target="_blank" rel="noopener noreferrer" data-kt="sourceOpen"></a></p>',
    '<p class="keep-error" data-kf="error" hidden></p>',
    '<footer class="keep-foot"><button type="button" class="keep-btn ghost" data-kf="cancel"></button>',
    '<button type="button" class="keep-btn primary" data-kf="save"><span class="keep-save-label" data-kt="save"></span><span class="keep-spin" hidden>' + ICON_SPIN + '</span></button></footer>',
    '</div>',
    '<div class="keep-saved" hidden>',
    '<div class="keep-sv-item keep-sv-check">' + ICON_CHECK + '<span data-kf="svCheck" data-kt="saved"></span></div>',
    '<p class="keep-sv-item keep-sv-dup" data-kf="svDup" hidden></p>',
    '<h5 class="keep-sv-item keep-sv-title" data-kf="svTitle"></h5>',
    '<p class="keep-sv-item keep-sv-summary" data-kf="svSummary"></p>',
    '<p class="keep-sv-item keep-sv-meta" data-kf="svMeta"></p>',
    '<div class="keep-sv-item keep-foot"><button type="button" class="keep-btn ghost" data-kf="close"></button>',
    '<button type="button" class="keep-btn primary" data-kf="done"></button></div>',
    '</div>',
    '</section></div>'
  ].join('');
  msgNode.insertAdjacentElement('afterend', panel);

  // 静态文案（data-kt = knowledge.* key）；来源链接文案含操作说明，单独置 title
  panel.querySelectorAll('[data-kt]').forEach((el) => {
    el.textContent = t('knowledge.' + el.dataset.kt);
  });
  const $ = (name) => panel.querySelector('[data-kf="' + name + '"]');
  // 按钮标签（data-kf 功能属性不带文案，单独赋值）
  $('cancel').textContent = t('knowledge.cancel');
  $('close').textContent = t('knowledge.close');
  $('done').textContent = t('knowledge.done');
  $('understanding').placeholder = t('knowledge.understandingPlaceholder');

  // 引用区：有划词逐条 blockquote；无划词整块让位只留说明行——
  // 「原文摘录」承诺的是页面证据，提问不是原文，不进摘录块（入库仍记 manual 来源）
  const quotesEl = $('quotes');
  if (ctx.quotes.length) {
    for (const q of ctx.quotes) {
      const bq = document.createElement('blockquote');
      bq.className = 'keep-quote';
      bq.textContent = '“' + q.text + '”';
      quotesEl.appendChild(bq);
    }
  } else {
    $('quoteField').querySelector('.keep-label').hidden = true;
    quotesEl.hidden = true;
    $('noQuote').textContent = t('knowledge.noQuoteNote');
    $('noQuote').hidden = false;
  }
  // 来源行：无页面 URL 时整行隐藏（manual 来源仍入库，仅不展示打不开的链接）；
  // 有真实页面标题时以标题为链接文案（悬停看全文），无标题回退 i18n 默认文案
  const pageUrl = (ctx.page && ctx.page.url) || '';
  if (!pageUrl) {
    $('sourceLink').closest('.keep-source').hidden = true;
  } else {
    const pageTitle = (ctx.page && ctx.page.title) || '';
    $('sourceLink').href = pageUrl;
    if (pageTitle) {
      $('sourceLink').textContent = pageTitle;
      $('sourceLink').title = pageTitle;
    }
  }

  openState = { panel, entryBtn, msgNode, ctx, saved: false, userScrolled: false };

  // Esc 立即收起（键盘路径零动画）；点取消为快速收起
  panel.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') {
      e.stopPropagation();
      closeKeepPanel(true);
    }
  });
  $('cancel').addEventListener('click', () => closeKeepPanel(false));
  $('save').addEventListener('click', () => doSave(ctx, $));
  $('close').addEventListener('click', () => closeKeepPanel(false));
  $('done').addEventListener('click', () => closeKeepPanel(false));

  // 草稿预填：有内存草稿（收起过且填过字）直接恢复跳过请求；否则走 AI 草稿
  const titleEl = $('title');
  const summaryEl = $('summary');
  const undEl = $('understanding');
  [summaryEl, undEl].forEach((el) => el.addEventListener('input', () => autoGrow(el)));
  const memo = editDrafts.get(ctx.originEventId);
  if (memo) {
    titleEl.value = memo.title;
    summaryEl.value = memo.summary;
    undEl.value = memo.understanding;
    setFieldLoading(panel, false);
    autoGrow(summaryEl); // 恢复路径同样需可见后测高
    titleEl.focus({ preventScroll: true });
  } else {
    startDraft(ctx, panel, $);
  }

  // 展开动画（grid-rows 0fr→1fr）。滚入必须等动画结束：0 高时 nearest 判定
  // "已可见"而跳过（打开后要手动下滑的根因）。transitionend 为主、300ms 定时
  // 兜底防吞事件，两路幂等（revealPanel 内再次判定可见性）。
  let expanded = false;
  const onExpanded = (e) => {
    if (e.target === panel && e.propertyName === 'grid-template-rows') reveal();
  };
  const reveal = () => {
    if (expanded) return;
    expanded = true;
    panel.removeEventListener('transitionend', onExpanded);
    revealPanel(panel);
  };
  panel.addEventListener('transitionend', onExpanded);
  setTimeout(reveal, 300);
  requestAnimationFrame(() => panel.classList.add('open'));
}

// startDraft AI 草稿：标题/提炼两格 shimmer，其余立即可编辑；ok=false 走降级
// （服务端已给兜底标题；网络层失败则本地用问题截断），淡橙提示非错误。
async function startDraft(ctx, panel, $) {
  setFieldLoading(panel, true);
  const degradedEl = $('degraded');
  degradedEl.hidden = true;
  const res = await fetchKnowledgeDraft({
    question: ctx.question,
    answer: ctx.answer,
    quote: ctx.quotes.length ? ctx.quotes[0].text : ''
  });
  // 面板可能在请求期间被关闭/切换：节点已脱文档则丢弃结果
  if (openState === null || openState.panel !== panel || !panel.isConnected) return;
  const fallback = digestTitle(ctx.question);
  const title = (res && res.title) || fallback;
  const summary = (res && res.summary) || '';
  const ok = !!(res && res.ok);
  $('title').value = title;
  $('summary').value = summary;
  // 先解除 loading 再测高：display:none 时 scrollHeight 恒为 0，会把高度钉死成 0px
  setFieldLoading(panel, false);
  autoGrow($('summary'));
  revealPanel(panel); // 草稿撑高面板后二次滚入（用户未手动滚动时）
  if (!ok) {
    degradedEl.textContent = t('knowledge.draftDegraded');
    degradedEl.hidden = false;
  }
  $('title').focus({ preventScroll: true });
}

function setFieldLoading(panel, on) {
  panel.querySelectorAll('[data-kf="titleField"], [data-kf="summaryField"]').forEach((f) => {
    f.dataset.loading = on ? 'true' : 'false';
  });
}

// doSave 保存：按钮 morph（blur 交叉淡化切「保存中…」），成功切只读视图
// （40ms 级联浮现），幂等命中加说明行；失败恢复按钮并保留全部已填内容。
async function doSave(ctx, $) {
  if (openState && openState.saved) return;
  const saveBtn = $('save');
  const errEl = $('error');
  errEl.hidden = true;
  const title = $('title').value.trim();
  if (!title) {
    errEl.textContent = t('knowledge.titleRequired');
    errEl.hidden = false;
    $('title').focus({ preventScroll: true });
    return;
  }
  saveBtn.classList.add('is-loading');
  saveBtn.disabled = true;
  const labelEl = saveBtn.querySelector('.keep-save-label');
  labelEl.textContent = t('knowledge.saving');
  try {
    const res = await saveKnowledge({
      title,
      aiSummary: $('summary').value,
      userUnderstanding: $('understanding').value,
      originSessionId: ctx.originSessionId,
      originEventId: ctx.originEventId,
      sources: buildSources(ctx)
    });
    // 已保存视图（响应 item 为准）
    const item = res.item || {};
    $('svTitle').textContent = item.title || $('title').value.trim();
    $('svSummary').textContent = item.aiSummary || $('summary').value;
    $('svMeta').textContent = t('knowledge.savedMeta');
    if (res.created === false) {
      $('svDup').textContent = t('knowledge.savedDupNote');
      $('svDup').hidden = false;
      $('svCheck').textContent = t('knowledge.savedDup');
    }
    const card = panel_card($);
    const savedView = card.querySelector('.keep-saved');
    savedView.hidden = false;
    card.querySelector('.keep-edit').hidden = true;
    card.querySelector('.keep-head').hidden = true;
    requestAnimationFrame(() => requestAnimationFrame(() => {
      card.classList.add('saved'); // 触发级联浮现
    }));
    if (openState) {
      openState.saved = true;
      markEntrySaved(openState.entryBtn); // 入口同步定格「已保存」，不再诱导重复保存
    }
    saveBtn.disabled = false;
    saveBtn.classList.remove('is-loading');
    labelEl.textContent = t('knowledge.save');
    editDrafts.delete(ctx.originEventId); // 已落库，内存草稿作废
  } catch (err) {
    saveBtn.disabled = false;
    saveBtn.classList.remove('is-loading');
    labelEl.textContent = t('knowledge.save');
    errEl.textContent = (err && err.message) || t('knowledge.saveFailed');
    errEl.hidden = false;
  }
}

// panel_card 取面板卡片根（doSave 内辅助）。
function panel_card($) {
  return $('save').closest('.keep-card');
}

// markEntrySaved 保存成功（含幂等命中）后同步入口按钮态：文案换「已保存」、
// is-done 暗态定格并禁用——与摘要主入口成功态一致（数据幂等由服务端兜底，
// 这里把「已保存」显式呈现，避免用户重走保存流程才得知结果）。
function markEntrySaved(btn) {
  if (!btn) return;
  btn.classList.add('is-done');
  btn.disabled = true;
  btn.innerHTML = ICON_BOOK_SM + '<span>' + t('knowledge.saved') + '</span>';
}

// buildSources 证据组装：有划词 → 逐条 quote 来源（URL 存真实页面）；
// 无划词轮次 → conversation 来源（对话轮次本身即依据，quote_text 恒空——
// 提问与回答都不是页面原文，不能伪装成摘录；服务端同样强制清空作兜底）。
function buildSources(ctx) {
  const pageTitle = (ctx.page && ctx.page.title) || '';
  if (ctx.quotes.length) {
    return ctx.quotes.map((q) => ({
      sourceType: 'quote',
      quoteText: q.text,
      sourceUrl: q.url || (ctx.page && ctx.page.url) || '',
      sourceTitle: pageTitle
    }));
  }
  return [{
    sourceType: 'conversation',
    quoteText: '',
    sourceUrl: (ctx.page && ctx.page.url) || '',
    sourceTitle: pageTitle
  }];
}

// closeKeepPanel 收起。instant=true（Esc）零动画；先落内存草稿（重开不丢字），
// 动画结束移除节点；入口按钮焦点归还（键盘路径）。
function closeKeepPanel(instant) {
  if (!openState) return;
  const { panel, entryBtn, ctx, saved } = openState;
  if (!saved) {
    editDrafts.set(ctx.originEventId, {
      title: panel.querySelector('[data-kf="title"]')?.value || '',
      summary: panel.querySelector('[data-kf="summary"]')?.value || '',
      understanding: panel.querySelector('[data-kf="understanding"]')?.value || ''
    });
  }
  openState = null;
  if (!saved) {
    // 已保存的入口按钮保持 is-done 禁用态（不可再发起保存），也不归还焦点
    entryBtn.disabled = false;
  }
  const remove = () => panel.remove();
  if (instant) {
    panel.classList.add('no-anim');
    panel.classList.remove('open');
    requestAnimationFrame(() => requestAnimationFrame(() => {
      remove();
      if (!saved) entryBtn.focus({ preventScroll: true });
    }));
  } else {
    panel.classList.remove('open');
    setTimeout(remove, 240); // 与 CSS 收起过渡 220ms 对齐
  }
}

// ---------- 小工具 ----------

// digestTitle 降级兜底标题：问题压空格后截 24 字（与服务端 fallbackTitle 同参）。
function digestTitle(question) {
  const q = String(question || '').replace(/\s+/g, ' ').trim();
  if (!q) return t('knowledge.titleLabel');
  return q.length > 24 ? q.slice(0, 24) : q;
}

// autoGrow textarea 自适应高度（input 即时生长，防滚动条）。
function autoGrow(el) {
  el.style.height = 'auto';
  el.style.height = el.scrollHeight + 'px';
}

// ---------- M3.1 关联卡片 ----------

// attachAssociations 在回答消息节点下方渲染低干扰关联卡片（后端有命中才调用）。
// 收起态仅一行（「与你之前的理解有关（N）」），展开逐条列出候选：
// 观点归属区分「你认为 / 你保存过」（userUnderstanding 为空 = 只保存过原文，
// 不得表述为个人观点）；每条附来源证据与「比较新旧理解」入口。
export function attachAssociations(msgNode, payload) {
  const cands = (payload && payload.candidates) || [];
  if (!cands.length) return;
  const card = document.createElement('div');
  card.className = 'assoc-card';
  const head = document.createElement('button');
  head.type = 'button';
  head.className = 'assoc-head';
  // 图标/箭头为静态常量，文案走 i18n，均可安全 innerHTML
  head.innerHTML = ICON_BOOK_SM + '<span class="assoc-head-label">'
    + t('knowledge.assocTitle', { n: cands.length }) + '</span><span class="assoc-caret">▾</span>';
  const body = document.createElement('div');
  body.className = 'assoc-body';
  for (const c of cands) {
    body.appendChild(buildAssocItem(c));
  }
  head.addEventListener('click', () => {
    const open = card.classList.toggle('open');
    head.querySelector('.assoc-caret').textContent = open ? '▴' : '▾';
  });
  card.appendChild(head);
  card.appendChild(body);
  msgNode.appendChild(card);
}

// buildAssocItem 单条候选：标题 + 观点归属行 + 来源证据 + 比较入口。
function buildAssocItem(c) {
  const item = document.createElement('div');
  item.className = 'assoc-item';
  const title = document.createElement('p');
  title.className = 'assoc-item-title';
  title.textContent = c.title || '';
  item.appendChild(title);
  // 观点归属：userUnderstanding 有值才说「你认为」，否则是「你保存过」
  const own = document.createElement('p');
  own.className = 'assoc-item-own';
  own.textContent = c.userUnderstanding
    ? t('knowledge.assocYouThink') + c.userUnderstanding
    : t('knowledge.assocYouSaved') + (c.aiSummary || '');
  item.appendChild(own);
  // 来源证据：quote 摘录优先，来源标题作链接文案；无证据不渲染该行
  const src = c.topSource;
  if (src && (src.quoteText || src.sourceTitle)) {
    const line = document.createElement('p');
    line.className = 'assoc-item-src';
    if (src.quoteText) {
      line.textContent = '“' + clipText(src.quoteText, 60) + '”';
    }
    if (src.sourceUrl) {
      const a = document.createElement('a');
      a.href = src.sourceUrl;
      a.target = '_blank';
      a.rel = 'noopener noreferrer';
      a.textContent = src.sourceTitle || t('knowledge.sourceOpen');
      line.appendChild(a);
    } else if (src.sourceTitle) {
      const span = document.createElement('span');
      span.textContent = src.sourceTitle;
      line.appendChild(span);
    }
    item.appendChild(line);
  }
  const actions = document.createElement('div');
  actions.className = 'assoc-item-actions';
  const cmp = document.createElement('button');
  cmp.type = 'button';
  cmp.className = 'assoc-compare';
  cmp.textContent = t('knowledge.assocCompare');
  cmp.addEventListener('click', () => prefillCompare(c.title || ''));
  actions.appendChild(cmp);
  item.appendChild(actions);
  return item;
}

// clipText 展示文本 rune 安全截断。
function clipText(s, n) {
  const r = Array.from(String(s || ''));
  return r.length > n ? r.slice(0, n).join('') + '…' : r.join('');
}

// prefillCompare 比较入口（M3.2 前的轻量形态）：把比较式提问预填进输入框
// 并聚焦光标到末尾，不直接发送——发不发、怎么问由用户决定（低干扰原则）。
function prefillCompare(title) {
  const el = document.getElementById('questionInput');
  if (!el) return;
  el.textContent = t('knowledge.assocComparePrompt', { title });
  el.dispatchEvent(new Event('input', { bubbles: true }));
  el.focus();
  const sel = window.getSelection();
  const range = document.createRange();
  range.selectNodeContents(el);
  range.collapse(false);
  sel.removeAllRanges();
  sel.addRange(range);
}
