// 在线真实网页整链路演示：打开站点 → 划词采集 → 两轮问答。
// 在线页内容不受控，断言只验证链路可用与 page 归属，不校验正文词（回归护栏由本地快照承担）。
// 默认跳过，演示跑法：
//   $env:ABSORB_E2E_LIVE='1'; $env:ABSORB_E2E_HEADED='1'; $env:ABSORB_E2E_SLOWMO='300'
//   npx playwright test specs/live-demo.spec.ts
// 目标页面可用 ABSORB_E2E_LIVE_URL 覆盖。
import { test, expect, MOCK_PORT, closeTestPages, type Page } from '../fixtures/extension.fixture';
import { selectNeedle } from '../fixtures/select-text';

const LIVE_URL = process.env.ABSORB_E2E_LIVE_URL
  || 'https://www.smallyoung.cn/docs/028-RAG%E6%B7%B7%E5%90%88%E6%A3%80%E7%B4%A2%E4%B8%8ERRF%E7%AE%97%E6%B3%95%E6%B7%B1%E5%BA%A6%E8%A7%A3%E6%9E%90';
const NEEDLE = 'RRF'; // 标题/正文主题词；找不到时 selectNeedle 自动兜底选可见长文本
const MAX_PAGE_TEXT_CHARS = 100_000; // grab.js 扩展端 pageMd 上限
const SLOWMO = Number(process.env.ABSORB_E2E_SLOWMO) || 0;

// 慢速演示：固定回复拆成多段 token 延迟送出，便于观测流式过程；常规跑用默认脚本
function demoAskScript(question: string) {
  if (!SLOWMO) return null;
  const tokens = ['E2E 回答：', `收到「${question}」`, '。这是', '多段流式', '输出演示，', '逐段送达。'];
  return [
    { type: 'session', sessionId: 'mock-session-1' },
    ...tokens.map((content) => ({ delayMs: 220, data: { type: 'token', content } })),
    { delayMs: 220, data: { done: true, requestId: 'mock-req-1' } },
  ];
}

async function setAskScript(events: unknown[] | null) {
  const resp = await fetch(`http://127.0.0.1:${MOCK_PORT}/__ask-script`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(events),
  });
  expect(resp.ok).toBe(true);
}

test.describe('在线真实网页演示', () => {
  test.skip(!process.env.ABSORB_E2E_LIVE, '设 ABSORB_E2E_LIVE=1 才跑（依赖外网与在线站点，不进常规套件）');
  test.setTimeout(180_000);

  let sidepanel: Page;

  test.beforeEach(async ({ context, serviceWorker }) => {
    await serviceWorker.evaluate(async () => {
      await (globalThis as { chrome: any }).chrome.storage.local.clear();
    });
    await setAskScript(null);
    sidepanel = await context.newPage();
    await sidepanel.goto(`chrome-extension://${new URL(serviceWorker.url()).host}/sidepanel/sidepanel.html`);
    await expect(sidepanel.locator('#questionInput')).toBeVisible();
  });

  test.afterEach(async ({ context }) => {
    await setAskScript(null);
    await closeTestPages(context);
  });

  test('整链路：打开真实站点 → 划词采集 → 两轮问答', async ({ context, serviceWorker }) => {
    // 在线页面不做请求拦截（真实站点依赖其静态资源渲染）
    const article = await context.newPage();
    await article.goto(LIVE_URL, { waitUntil: 'domcontentloaded', timeout: 60_000 });

    await selectNeedle(article, NEEDLE);
    const tabId = await serviceWorker.evaluate(async (u: string) => {
      const tabs = await (globalThis as { chrome: any }).chrome.tabs.query({});
      return tabs.find((t: { url?: string }) => t.url === u)?.id;
    }, LIVE_URL);
    await sidepanel.evaluate(async (id: number) => {
      await (globalThis as { chrome: any }).chrome.runtime.sendMessage({
        type: 'test-menu-click',
        skipPanelOpen: true, // E2E 不开原生面板
        info: { menuItemId: 'absorb-open-panel', selectionText: '在线页划词', frameId: 0 },
        tab: { id, url: 'x' },
      });
    }, tabId);
    await expect(sidepanel.locator('.quote-chip')).toHaveCount(1);

    // 页面置前台（整页材料归属该页）→ 第一轮提问
    await article.bringToFront();
    await expect.poll(async () => serviceWorker.evaluate(async (u: string) => {
      const tabs = await (globalThis as { chrome: any }).chrome.tabs.query({ active: true, currentWindow: true });
      return tabs[0]?.url;
    }, LIVE_URL), { timeout: 10_000 }).toBe(LIVE_URL);

    const question = '这篇讲了什么？';
    if (SLOWMO) await setAskScript(demoAskScript(question));
    await sidepanel.locator('#questionInput').fill(question);
    await sidepanel.locator('#askBtn').click();

    let payload: any;
    await expect.poll(async () => {
      payload = await sidepanel.evaluate(async (port) =>
        (await (await fetch(`http://127.0.0.1:${port}/__last-ask`)).json()).lastAsk, MOCK_PORT);
      return payload?.question?.includes(question);
    }, { timeout: 20_000 }).toBe(true);
    expect(payload.page.url, 'page 归属应为在线页').toBe(LIVE_URL);
    const pageMd = String(payload.page.pageMd || '');
    expect(pageMd.length, 'pageMd 非空').toBeGreaterThan(0);
    expect(pageMd.length, 'pageMd 不超扩展端上限').toBeLessThanOrEqual(MAX_PAGE_TEXT_CHARS);
    await expect(sidepanel.locator('.msg.assistant')).toBeVisible();
    await expect(sidepanel.locator('.msg.assistant.streaming')).toHaveCount(0);

    // 第二轮：同会话追问
    const followUp = 'RAG 里为什么要做混合检索？';
    if (SLOWMO) await setAskScript(demoAskScript(followUp));
    await sidepanel.locator('#questionInput').fill(followUp);
    await sidepanel.locator('#askBtn').click();
    await expect.poll(async () => {
      const latest = await sidepanel.evaluate(async (port) =>
        (await (await fetch(`http://127.0.0.1:${port}/__last-ask`)).json()).lastAsk, MOCK_PORT);
      return latest?.question;
    }, { timeout: 20_000 }).toBe(followUp);
    await expect(sidepanel.locator('.msg.assistant')).toHaveCount(2);
    await expect(sidepanel.locator('.msg.assistant.streaming')).toHaveCount(0);
  });
});
