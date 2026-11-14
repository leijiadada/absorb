// 划词待取队列：append-only，sidepanel 按 id 游标消费，不覆盖。
// 单调递增 id，保证 sidepanel 游标比较可靠；SW 重启后 _lastId 归零，Date.now()
// 仍单调递增，同毫秒冲突仅在极端重启场景出现，可忽略。

// 队列条数上限：只兜底防膨胀，正常消费节奏下队列长度是个位数
const MAX_PENDING_QUOTES = 30;

// 敏感日志开关：跳过提示含划词文本，发布版保持静默，排查时临时打开
const DEBUG = false;

let _lastId = 0;

function nextQuoteId() {
  let id = Date.now();
  if (id <= _lastId) id = _lastId + 1;
  _lastId = id;
  return id;
}

// 把一次划词追加到待消费队列。只 append 不清除队列本身（避免与 sidepanel 消费竞争导致覆盖）。
// 查重只针对"尚未被 sidepanel 消费成标签"的同文划词(其 id 仍大于消费游标)，避免同一段连续右键堆积；
// 已被消费(成标签后又删除)的同文划词 id <= 游标，放行重新入队，保证删除后仍能再次引用。
export async function appendPendingQuote(quote) {
  if (!quote || !quote.text) return;
  const data = await chrome.storage.local.get(['pendingQuotes', 'pendingCursor']);
  const list = Array.isArray(data.pendingQuotes) ? data.pendingQuotes : [];
  const cursor = Number(data.pendingCursor) || 0;
  const pendingDup = list.some((q) =>
    q.url === quote.url && q.text === quote.text && typeof q.id === 'number' && q.id > cursor);
  if (pendingDup) {
    if (DEBUG) console.log('[Absorb] 存在未消费的同文划词，跳过入队:', quote.text);
    return;
  }
  list.push({
    id: nextQuoteId(),
    text: quote.text,
    paragraph: (quote.paragraph || ''),
    pageContext: (quote.pageContext || ''),
    url: (quote.url || ''),
    pageMd: (quote.pageMd || '')
  });
  if (list.length > MAX_PENDING_QUOTES) list.splice(0, list.length - MAX_PENDING_QUOTES);
  try {
    await chrome.storage.local.set({ pendingQuotes: list });
  } catch (e) {
    // 配额溢出等写入失败：剥离队列内 pageMd 重试一次保住划词文本，再失败只能放弃（日志可见）
    console.error('[Absorb] 划词入队失败，剥离 pageMd 降级重试:', e);
    try {
      await chrome.storage.local.set({ pendingQuotes: list.map((q) => ({ ...q, pageMd: '' })) });
    } catch (e2) {
      console.error('[Absorb] 划词入队仍失败（存储配额耗尽？）:', e2);
      return;
    }
  }
  console.log('[Absorb] 划词已入队，当前队列:', list.length);
}
