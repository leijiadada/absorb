// 单页多引用提交链路：两次真实划词 → 两枚 chip → 提交后 question 只含锚点前缀与
// 用户问题（无材料拼接），全部引用文本以 quoteTexts 数组随 context 上送（主源仍取第一条）。
// 跨页引用（方案 A）不拦截：context 页级字段仍为第一条引用来源页。
// 划词经 background 的 test-menu-click 钩子，与右键菜单完全同链（见 content-capture.spec）。
import { test, expect, FIXTURE_BASE, MOCK_PORT, closeTestPages, type Page, type BrowserContext, type Worker } from '../fixtures/extension.fixture';

const ARTICLE_A_URL = `${FIXTURE_BASE}/article-a.html`;
const ARTICLE_B_URL = `${FIXTURE_BASE}/article-b.html`;
const FRAGMENT_A = '为大语言模型提供外部上下文';
const FRAGMENT_B = '答案可溯源';
const FRAGMENT_C = '混合检索并辅以重排序';

type AnyChrome = { chrome: any };

// 在页面可见文本节点里定位关键词并构造真实选区（与鼠标拖选语义等价）
async function selectFragment(page: Page, keyword: string) {
  await page.evaluate((kw) => {
    const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
    let node: Text | null;
    while ((node = walker.nextNode() as Text | null)) {
      const idx = (node.textContent || '').indexOf(kw);
      if (idx < 0 || !(node.parentElement?.getClientRects().length ?? 0)) continue;
      const range = document.createRange();
      range.setStart(node, idx);
      range.setEnd(node, idx + kw.length);
      const sel = window.getSelection()!;
      sel.removeAllRanges();
      sel.addRange(range);
      return;
    }
    throw new Error('keyword not found: ' + kw);
  }, keyword);
}

// 经扩展页面发 test-menu-click（SW 不接收自身 sendMessage，必须由扩展页面发起）
async function triggerMenuClick(extPage: Page, tabId: number, fragment: string, url: string) {
  return extPage.evaluate(async ({ tabId, fragment, url }) => {
    return (globalThis as unknown as AnyChrome).chrome.runtime.sendMessage({
      type: 'test-menu-click',
      skipPanelOpen: true,
      info: { menuItemId: 'absorb-open-panel', selectionText: fragment, frameId: 0 },
      tab: { id: tabId, url },
    });
  }, { tabId, fragment, url });
}

async function getTabId(sw: Worker, urlPart: string) {
  return sw.evaluate(async (part: string) => {
    const tabs = await (globalThis as unknown as AnyChrome).chrome.tabs.query({});
    const tab = tabs.find((t: { url?: string }) => (t.url || '').includes(part));
    return (tab?.id ?? -1) as number;
  }, urlPart);
}

// 清空 mock 后端记录的最近一次 /api/ask payload
async function resetLastAsk(port: number) {
  const resp = await fetch(`http://127.0.0.1:${port}/__ask-script`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(null),
  });
  expect(resp.ok).toBe(true);
}

async function pollLastAsk(sidepanel: Page, port: number) {
  await expect.poll(async () => {
    const r = await sidepanel.evaluate(async (p) => {
      return (await (await fetch(`http://127.0.0.1:${p}/__last-ask`)).json())?.lastAsk ?? null;
    }, port);
    return r;
  }, { timeout: 10_000 }).not.toBeNull();
  return sidepanel.evaluate(async (p) => {
    return (await (await fetch(`http://127.0.0.1:${p}/__last-ask`)).json())?.lastAsk;
  }, port);
}

test.describe('单页多引用', () => {
  let sidepanel: Page;
  let pageA: Page;

  test.beforeEach(async ({ context, serviceWorker }) => {
    await serviceWorker.evaluate(async () => {
      await (globalThis as unknown as AnyChrome).chrome.storage.local.clear();
    });
    await resetLastAsk(MOCK_PORT);
    sidepanel = await context.newPage();
    await sidepanel.goto(`chrome-extension://${new URL(serviceWorker.url()).host}/sidepanel/sidepanel.html`);
    await expect(sidepanel.locator('#questionInput')).toBeVisible();
    pageA = await context.newPage();
    await pageA.goto(ARTICLE_A_URL);
    await pageA.bringToFront();
  });

  test.afterEach(async ({ context }) => {
    await resetLastAsk(MOCK_PORT);
    await closeTestPages(context);
  });

  test('同页双划词：quoteTexts 按序上送，question 无材料拼接', async ({ serviceWorker }) => {
    const tabId = await getTabId(serviceWorker, 'article-a.html');
    expect(tabId).toBeGreaterThan(0);

    await selectFragment(pageA, FRAGMENT_A);
    expect(await triggerMenuClick(sidepanel, tabId, FRAGMENT_A, ARTICLE_A_URL)).toEqual({ ok: true });
    await selectFragment(pageA, FRAGMENT_B);
    expect(await triggerMenuClick(sidepanel, tabId, FRAGMENT_B, ARTICLE_A_URL)).toEqual({ ok: true });

    // 两枚 chip 按划词顺序就位
    await expect(sidepanel.locator('.quote-chip .chip-text[title="' + FRAGMENT_A + '"]')).toBeVisible();
    await expect(sidepanel.locator('.quote-chip .chip-text[title="' + FRAGMENT_B + '"]')).toBeVisible();

    // 消费完成后光标停在末枚 chip 右侧，直接续打；chip 在 composer 序列化为 [[n]] 前缀，
    // net.js 替换为【引用n】锚点（与 [[n]] 关联编号设计一致）
    await sidepanel.locator('#questionInput').pressSequentially('这两段引用有什么区别');
    const askBtn = sidepanel.locator('#askBtn');
    await expect(askBtn).toBeEnabled();
    await askBtn.click();

    const lastAsk = await pollLastAsk(sidepanel, MOCK_PORT);
    expect(lastAsk.question).toBe('【引用1】【引用2】这两段引用有什么区别');
    expect(lastAsk.context.quoteTexts).toEqual([FRAGMENT_A, FRAGMENT_B]); // 序号=chip 顺序
    expect(lastAsk.context.text).toBe(FRAGMENT_A); // 主源取第一条
    expect(lastAsk.context.pageMd).toContain('absorb-fixture-alpha-marker'); // 首条 pageMd 完整（入队去重后消费回填）
    expect(lastAsk.context.url).toBe(ARTICLE_A_URL);
  });

  test('跨页双划词（方案 A 不拦截）：请求形状不变，页级字段随第一条', async ({ context, serviceWorker }) => {
    const tabIdA = await getTabId(serviceWorker, 'article-a.html');
    await selectFragment(pageA, FRAGMENT_A);
    expect(await triggerMenuClick(sidepanel, tabIdA, FRAGMENT_A, ARTICLE_A_URL)).toEqual({ ok: true });

    const pageB = await context.newPage();
    await pageB.goto(ARTICLE_B_URL);
    await pageB.bringToFront();
    const tabIdB = await getTabId(serviceWorker, 'article-b.html');
    await selectFragment(pageB, FRAGMENT_C);
    expect(await triggerMenuClick(sidepanel, tabIdB, FRAGMENT_C, ARTICLE_B_URL)).toEqual({ ok: true });

    await expect(sidepanel.locator('.quote-chip .chip-text[title="' + FRAGMENT_A + '"]')).toBeVisible();
    await expect(sidepanel.locator('.quote-chip .chip-text[title="' + FRAGMENT_C + '"]')).toBeVisible();

    // 消费完成后光标停在末枚 chip 右侧，直接续打
    await sidepanel.locator('#questionInput').pressSequentially('两段分别讲了什么');
    const askBtn = sidepanel.locator('#askBtn');
    await expect(askBtn).toBeEnabled();
    await askBtn.click();

    const lastAsk = await pollLastAsk(sidepanel, MOCK_PORT);
    expect(lastAsk.question).toBe('【引用1】【引用2】两段分别讲了什么');
    expect(lastAsk.context.quoteTexts).toEqual([FRAGMENT_A, FRAGMENT_C]);
    expect(lastAsk.context.url).toBe(ARTICLE_A_URL); // 页级字段仍为第一条来源页
  });
});
