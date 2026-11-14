// P0-2：划词采集。经 background.js 的 test-menu-click 钩子，走与右键菜单完全相同的
// 采集链：handleMenuClick → executeScript(grab.js) → appendPendingQuote → 面板实时消费。
// 划词本身用 Range API 在真实页面构造选区（与鼠标拖选等价）。
import { test, expect, FIXTURE_BASE, closeTestPages } from '../fixtures/extension.fixture';

const FRAGMENT = '为大语言模型提供外部上下文'; // 短句无句末标点 → isComplete=false → 抓段落
const PARAGRAPH = 'RAG 系统通过检索相关资料，为大语言模型提供外部上下文，从而显著降低模型幻觉。';
const ARTICLE_URL = `${FIXTURE_BASE}/article-a.html`;
const PANEL_MENU_ID = 'absorb-open-panel'; // 与 quote-collector.js 的 PANEL_MENU_ID 一致

type AnyChrome = { chrome: any };

// 打开 fixture 文章页并用 Range API 构造真实选区
async function openArticleAndSelect(context: import('@playwright/test').BrowserContext) {
  const page = await context.newPage();
  await page.goto(ARTICLE_URL);
  await page.evaluate(() => {
    const p = document.querySelector('#content')!;
    const node = p.firstChild!;
    const full = node.textContent || '';
    const start = full.indexOf('为大语言模型');
    const range = document.createRange();
    range.setStart(node, start);
    range.setEnd(node, start + '为大语言模型提供外部上下文'.length);
    const sel = window.getSelection()!;
    sel.removeAllRanges();
    sel.addRange(range);
  });
  return page;
}

// 经扩展页面发 test-menu-click（SW 不接收自身 sendMessage，必须由扩展页面发起）
async function triggerMenuClick(extPage: import('@playwright/test').Page, tabId: number) {
  return extPage.evaluate(async ({ tabId, fragment, url }) => {
    const resp = await (globalThis as unknown as AnyChrome).chrome.runtime.sendMessage({
      type: 'test-menu-click',
      skipPanelOpen: true, // E2E 不开原生面板
      info: { menuItemId: 'absorb-open-panel', selectionText: fragment, frameId: 0 },
      tab: { id: tabId, url },
    });
    return resp;
  }, { tabId, fragment: FRAGMENT, url: ARTICLE_URL });
}

async function getTabId(sw: import('@playwright/test').Worker, urlPart: string) {
  return sw.evaluate(async (part: string) => {
    const tabs = await (globalThis as unknown as AnyChrome).chrome.tabs.query({});
    const tab = tabs.find((t: { url?: string }) => (t.url || '').includes(part));
    return (tab?.id ?? -1) as number;
  }, urlPart);
}

async function readPendingQuotes(sw: import('@playwright/test').Worker) {
  return sw.evaluate(async () => {
    const data = await (globalThis as unknown as AnyChrome).chrome.storage.local.get(['pendingQuotes']);
    return (data.pendingQuotes || []) as Array<Record<string, unknown>>;
  });
}

test.describe('划词采集', () => {
  let sidepanel: import('@playwright/test').Page;

  test.beforeEach(async ({ context, serviceWorker }) => {
    await serviceWorker.evaluate(async () => {
      await (globalThis as unknown as AnyChrome).chrome.storage.local.clear();
    });
    sidepanel = await context.newPage();
    await sidepanel.goto(`chrome-extension://${new URL(serviceWorker.url()).host}/sidepanel/sidepanel.html`);
    await expect(sidepanel.locator('#questionInput')).toBeVisible();
  });

  test.afterEach(async ({ context }) => {
    await closeTestPages(context);
  });

  test('采集全链路：真实页面选区 → 入队字段正确 → 面板实时出 chip', async ({
    context, serviceWorker,
  }) => {
    const article = await openArticleAndSelect(context);
    const tabId = await getTabId(serviceWorker, 'article-a.html');
    expect(tabId).toBeGreaterThan(0);

    // sendMessage 在 handleMenuClick（含 appendPendingQuote）完成后才 resolve
    const resp = await triggerMenuClick(sidepanel, tabId);
    expect(resp).toEqual({ ok: true });

    // 入队字段断言（storage.local / pendingQuotes）
    const quotes = await readPendingQuotes(serviceWorker);
    expect(quotes.length).toBe(1);
    const q = quotes[0];
    expect(String(q.text)).toContain(FRAGMENT);
    expect(String(q.paragraph)).toBe(PARAGRAPH);
    expect(String(q.url)).toBe(ARTICLE_URL);
    expect(String(q.pageMd)).toContain('absorb-fixture-alpha-marker');
    // 注：isComplete 只影响 grab 是否抓段落，不随 quote 持久化（存储形状见 quote-queue.js）
    expect(Object.keys(q).sort()).toEqual(['id', 'pageContext', 'pageMd', 'paragraph', 'text', 'url']);

    // 面板消费：storage.onChanged → 输入框出现 quote chip（chip 可见文本会被 chipText 截断，
    // 全文在 .chip-text 的 title 上，按 title 定位）
    await expect(
      sidepanel.locator(`.quote-chip .chip-text[title="${FRAGMENT}"]`),
    ).toBeVisible();
  });

  test('无划词只开面板：不入队、不出 chip', async ({ context, serviceWorker }) => {
    await context.newPage().then((p) => p.goto(ARTICLE_URL)); // 仅提供可注入的 tab
    const tabId = await getTabId(serviceWorker, 'article-a.html');

    const extPage = sidepanel;
    const resp = await extPage.evaluate(async ({ tabId, url }) => {
      return await (globalThis as unknown as AnyChrome).chrome.runtime.sendMessage({
        type: 'test-menu-click',
        skipPanelOpen: true,
        info: { menuItemId: 'absorb-open-panel', selectionText: '', frameId: 0 },
        tab: { id: tabId, url },
      });
    }, { tabId, url: ARTICLE_URL });
    expect(resp).toEqual({ ok: true });

    const quotes = await readPendingQuotes(serviceWorker);
    expect(quotes.length).toBe(0);
    await expect(sidepanel.locator('.quote-chip')).toHaveCount(0);
  });
});
