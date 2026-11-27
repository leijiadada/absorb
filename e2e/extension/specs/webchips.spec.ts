// 网页建议 chips（doc-suggestions §4.4）：普通网页（非注册文档类型）空态区
// 显示 pageMd 标题直出的成句问题；PDF 活动页时 chips 隐藏（与文档卡互斥）。
// 提交提问后当前页 chips 隐藏（notifyAsked），换页后按新页重新出现。
// 素材链路：webchips.js 向 fixture 页注入 grab.js 采集 pageMd → 抽 #/## 标题
// （store-article.html 含 1×h1 + 2×h2 = 3 个标题，满足 ≥2 条渲染门槛，上限截前 2 条）。
// 提问链路：点击落输入框提交 → 无划词采集当前页 → /api/ask 注入 pageMd；
// 回答基于页面正文（不经 search_document，未 prepare）。
// 功能暂时下线（webchips.js WEB_CHIPS_ENABLED = false），恢复展示时移除 .skip。
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

test.describe.skip('网页建议 chips', () => {
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
    // 文档卡不出现（非注册文档类型）；chips 渲染（pageMd 抽得 3 个标题，上限截前 2 条）
    await expect(sidepanel.locator('#docCard')).toBeHidden();
    await expect(sidepanel.locator('#webChips .wc-chip')).toHaveCount(2);
    // 点击第一条：问题为客户端组句的成句文本（末级标题来自 pageMd 标题行）
    await sidepanel.locator('#webChips .wc-chip').first().click();
    await expect(sidepanel.locator('.msg.assistant').last()).toBeVisible();
    await expect.poll(async () => sidepanel.evaluate(async (port) => {
      const r = await fetch(`http://127.0.0.1:${port}/__last-ask`);
      return (await r.json())?.lastAsk?.question ?? '';
    }, MOCK_PORT), { timeout: 10_000 }).toContain('文中〈');
    // 提问成立后当前页 chips 隐藏（方案 A：提问后隐藏，换页再按新页重新出现）
    await expect(sidepanel.locator('#webChips')).toBeHidden();
  });

  test('提问后 chips 隐藏，换页后再回到原页重新出现', async ({ serviceWorker }) => {
    await expect(sidepanel.locator('#webChips .wc-chip')).toHaveCount(2);
    await sidepanel.locator('#webChips .wc-chip').first().click();
    await expect(sidepanel.locator('.msg.assistant').last()).toBeVisible();
    // 当前页隐藏；同页后续刷新事件（标题变化/重载）不恢复
    await expect(sidepanel.locator('#webChips')).toBeHidden();
    await page.reload();
    await expectActive(serviceWorker, PAGE_URL);
    await expect(sidepanel.locator('#webChips')).toBeHidden();
    // 切到仅 1 个标题的页（低于 ≥2 渲染门槛，无 chips），再切回原页：解除隐藏并重新组句
    await page.goto(`${FIXTURE_BASE}/article-a.html`);
    await expectActive(serviceWorker, `${FIXTURE_BASE}/article-a.html`);
    await expect(sidepanel.locator('#webChips')).toBeHidden();
    await page.goto(PAGE_URL);
    await expectActive(serviceWorker, PAGE_URL);
    await expect(sidepanel.locator('#webChips .wc-chip')).toHaveCount(2);
  });

  test('直接输入提问 → chips 同步隐藏', async () => {
    await expect(sidepanel.locator('#webChips .wc-chip')).toHaveCount(2);
    // composer 无 chip 时 fill 安全；输入文本点提问按钮，走与 chip 点击同一 submitQuestion 入口
    await sidepanel.locator('#questionInput').fill('直接提问验证：webchips 应隐藏');
    const askBtn = sidepanel.locator('#askBtn');
    await expect(askBtn).toBeEnabled();
    await askBtn.click();
    await expect(sidepanel.locator('.msg.assistant').last()).toBeVisible();
    await expect.poll(async () => sidepanel.evaluate(async (port) => {
      const r = await fetch(`http://127.0.0.1:${port}/__last-ask`);
      return (await r.json())?.lastAsk?.question ?? '';
    }, MOCK_PORT), { timeout: 10_000 }).toContain('直接提问验证');
    await expect(sidepanel.locator('#webChips')).toBeHidden();
  });

  test('划词提问 → chips 同步隐藏', async ({ context, serviceWorker }) => {
    await expect(sidepanel.locator('#webChips .wc-chip')).toHaveCount(2);
    // 在活动页构造真实选区 → test-menu-click 走右键菜单同款采集链 → 面板实时出 chip
    await page.evaluate(() => {
      const p = document.querySelector('.article p')!;
      const node = p.firstChild!;
      const start = (node.textContent || '').indexOf('头三天是抗拒的');
      const range = document.createRange();
      range.setStart(node, start);
      range.setEnd(node, start + '头三天是抗拒的'.length);
      const sel = window.getSelection()!;
      sel.removeAllRanges();
      sel.addRange(range);
    });
    const tabId = await serviceWorker.evaluate(async () => {
      const tabs = await (globalThis as AnyChrome).chrome.tabs.query({ active: true, currentWindow: true });
      return tabs[0]?.id ?? -1;
    });
    expect(tabId).toBeGreaterThan(0);
    const resp = await sidepanel.evaluate(async ({ tabId, url }) => {
      return await (globalThis as AnyChrome).chrome.runtime.sendMessage({
        type: 'test-menu-click',
        skipPanelOpen: true,
        info: { menuItemId: 'absorb-open-panel', selectionText: '头三天是抗拒的', frameId: 0 },
        tab: { id: tabId, url },
      });
    }, { tabId, url: PAGE_URL });
    expect(resp).toEqual({ ok: true });
    await expect(sidepanel.locator('.quote-chip')).toBeVisible();
    // chip 落框后直接续打文本提交（与 multi-quote 同款操作），提问成立即隐藏当前页 chips
    await sidepanel.locator('#questionInput').pressSequentially('划词提问验证：webchips 应隐藏');
    const askBtn = sidepanel.locator('#askBtn');
    await expect(askBtn).toBeEnabled();
    await askBtn.click();
    await expect(sidepanel.locator('.msg.assistant').last()).toBeVisible();
    await expect.poll(async () => sidepanel.evaluate(async (port) => {
      const r = await fetch(`http://127.0.0.1:${port}/__last-ask`);
      return (await r.json())?.lastAsk?.question ?? '';
    }, MOCK_PORT), { timeout: 10_000 }).toContain('划词提问验证');
    await expect(sidepanel.locator('#webChips')).toBeHidden();
  });

  test('PDF 活动页时 chips 隐藏，文档卡出现（互斥）', async ({ context, serviceWorker }) => {
    await expect(sidepanel.locator('#webChips .wc-chip')).toHaveCount(2);
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
