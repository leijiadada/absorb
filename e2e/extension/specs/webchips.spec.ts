// 网页建议 chips（doc-suggestions §4.4）：普通网页（非注册文档类型）空态区
// 显示 pageMd 标题直出的成句问题；PDF 活动页时 chips 隐藏（与文档卡互斥）。
// 素材链路：webchips.js 向 fixture 页注入 grab.js 采集 pageMd → 抽 #/## 标题
// （store-article.html 含 1×h1 + 2×h2 = 3 个标题，满足 ≥2 条渲染门槛）。
// 提问链路：点击落输入框提交 → 无划词采集当前页 → /api/ask 注入 pageMd；
// 回答基于页面正文（不经 search_document，未 prepare）。
import { test, expect, FIXTURE_BASE, MOCK_PORT, closeTestPages, type Page, type BrowserContext, type Worker } from '../fixtures/extension.fixture';

const PAGE_URL = `${FIXTURE_BASE}/store-article.html`;
const PDF_URL = `${FIXTURE_BASE}/sample.pdf`;

type AnyChrome = { chrome: any };

async function expectActive(serviceWorker: Worker, url: string) {
  await expect.poll(async () => serviceWorker.evaluate(async () => {
    const tabs = await (globalThis as AnyChrome).chrome.tabs.query({ active: true, currentWindow: true });
    return tabs[0]?.url || '';
  }), { timeout: 10_000 }).toBe(url);
}

test.describe('网页建议 chips', () => {
  let sidepanel: Page;
  let page: Page;

  test.beforeEach(async ({ context, serviceWorker }) => {
    await serviceWorker.evaluate(async () => {
      await (globalThis as AnyChrome).chrome.storage.local.clear();
    });
    sidepanel = await context.newPage();
    await sidepanel.goto(`chrome-extension://${new URL(serviceWorker.url()).host}/sidepanel/sidepanel.html`);
    await expect(sidepanel.locator('#questionInput')).toBeVisible();
    page = await context.newPage();
    await page.goto(PAGE_URL);
    await page.bringToFront();
    await expectActive(serviceWorker, PAGE_URL);
  });

  test.afterEach(async ({ context }) => {
    await closeTestPages(context);
  });

  test('普通网页空态显示标题 chips，点击走提问链路', async () => {
    // 文档卡不出现（非注册文档类型）；chips 渲染（pageMd 1×h1+2×h2 = 3 条）
    await expect(sidepanel.locator('#docCard')).toBeHidden();
    await expect(sidepanel.locator('#webChips .wc-chip')).toHaveCount(3);
    // 点击第一条：问题为客户端组句的成句文本（末级标题来自 pageMd 标题行）
    await sidepanel.locator('#webChips .wc-chip').first().click();
    await expect(sidepanel.locator('.msg.assistant').last()).toBeVisible();
    await expect.poll(async () => sidepanel.evaluate(async (port) => {
      const r = await fetch(`http://127.0.0.1:${port}/__last-ask`);
      return (await r.json())?.lastAsk?.question ?? '';
    }, MOCK_PORT), { timeout: 10_000 }).toContain('文中〈');
    // 提交后问题块常驻输入区上方（与消息有无解耦），可继续点击追问
    await expect(sidepanel.locator('#webChips .wc-chip')).toHaveCount(3);
  });

  test('PDF 活动页时 chips 隐藏，文档卡出现（互斥）', async ({ context, serviceWorker }) => {
    await expect(sidepanel.locator('#webChips .wc-chip')).toHaveCount(3);
    const pdf = await context.newPage();
    await pdf.route('**/sample.pdf', (route) => route.fulfill({
      contentType: 'text/html; charset=utf-8',
      body: '<!DOCTYPE html><html><head><title>fake pdf viewer</title></head><body>fake pdf viewer</body></html>',
    }));
    await pdf.goto(PDF_URL);
    await pdf.bringToFront();
    await expectActive(serviceWorker, PDF_URL);
    await expect(sidepanel.locator('#webChips')).toBeHidden();
    await expect(sidepanel.locator('#docCard')).toBeVisible();
  });
});
