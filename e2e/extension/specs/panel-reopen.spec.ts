// P1-1：面板重开。真实场景是用户关掉 Side Panel 再打开——storage 持久化必须完整：
// 会话历史恢复渲染、当前会话指针不漂移、未发送草稿（含引用 chip）按 qid 回填。
// 这里用「关闭面板标签页 → 重开 sidepanel.html」模拟重开（哨兵页保证窗口不归零）。
import { test, expect, FIXTURE_BASE, MOCK_PORT, closeTestPages, type Page, type BrowserContext, type Worker } from '../fixtures/extension.fixture';

const URL_A = `${FIXTURE_BASE}/article-a.html`;
const FRAGMENT = '为大语言模型提供外部上下文';
const QUESTION = '重开前的提问';
const DRAFT_TEXT = '未发送的草稿内容';

test.describe('面板重开', () => {
  let sidepanel: Page;

  test.beforeEach(async ({ context, serviceWorker }) => {
    await serviceWorker.evaluate(async () => {
      await (globalThis as { chrome: any }).chrome.storage.local.clear();
    });
    await fetch(`http://127.0.0.1:${MOCK_PORT}/__ask-script`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(null),
    });
    sidepanel = await context.newPage();
    await sidepanel.goto(`chrome-extension://${new URL(serviceWorker.url()).host}/sidepanel/sidepanel.html`);
    await expect(sidepanel.locator('#questionInput')).toBeVisible();
  });

  test.afterEach(async ({ context }) => {
    await closeTestPages(context);
  });

  test('关闭重开 → 会话历史恢复、草稿与 chip 回填、当前会话指针不漂移', async ({
    context, serviceWorker,
  }) => {
    // 先完成一轮问答（无引用）——历史恢复的素材
    await sidepanel.locator('#questionInput').fill(QUESTION);
    await sidepanel.locator('#askBtn').click();
    await expect(sidepanel.locator('.msg.user').filter({ hasText: QUESTION })).toBeVisible();
    await expect(sidepanel.locator('.msg.assistant').filter({ hasText: 'mock 后端固定回复' })).toBeVisible();
    await expect(sidepanel.locator('.msg.assistant.streaming')).toHaveCount(0);

    // 划词采集出 chip（模拟"问完后又划了词"）
    const article = await context.newPage();
    await article.goto(URL_A);
    await article.evaluate((frag) => {
      const p = document.querySelector('#content')!;
      const node = p.firstChild!;
      const full = node.textContent || '';
      const start = full.indexOf(frag.slice(0, 6));
      const range = document.createRange();
      range.setStart(node, start);
      range.setEnd(node, start + frag.length);
      const sel = window.getSelection()!;
      sel.removeAllRanges();
      sel.addRange(range);
    }, FRAGMENT);
    const tabId = await serviceWorker.evaluate(async (url: string) => {
      const tabs = await (globalThis as { chrome: any }).chrome.tabs.query({});
      return tabs.find((t: { url?: string }) => t.url === url)?.id;
    }, URL_A);
    await sidepanel.evaluate(async ({ id, sel }) => {
      await (globalThis as { chrome: any }).chrome.runtime.sendMessage({
        type: 'test-menu-click',
        skipPanelOpen: true, // E2E 不开原生面板
        info: { menuItemId: 'absorb-open-panel', selectionText: sel, frameId: 0 },
        tab: { id, url: 'x' },
      });
    }, { id: tabId, sel: FRAGMENT });
    await expect(sidepanel.locator('.quote-chip')).toHaveCount(1);

    // 追加一段未发送草稿（不发送），等防抖落盘（快照含 chip 占位与 refs）
    await sidepanel.evaluate((t) => {
      const input = document.querySelector('#questionInput')!;
      input.appendChild(document.createTextNode(t));
      input.dispatchEvent(new InputEvent('input', { bubbles: true }));
    }, DRAFT_TEXT);
    // 注意：expect.poll 回调抛异常不重试——300ms 防抖窗口内 d 为 undefined，
    // 必须空值安全比较（JSON.stringify(undefined) 返回 undefined，.includes 会炸）
    await expect.poll(async () => {
      const d = await serviceWorker.evaluate(async () => {
        const c = (globalThis as { chrome: any }).chrome;
        return (await c.storage.local.get(['absorb-draft']))['absorb-draft'];
      });
      return String(d ?? '').includes(DRAFT_TEXT);
    }, { timeout: 5_000 }).toBe(true);

    // 记录当前会话指针 → 关面板 → 重开
    const sessionIdBefore = await serviceWorker.evaluate(async () => {
      const c = (globalThis as { chrome: any }).chrome;
      return (await c.storage.local.get(['absorb-state']))['absorb-state'].currentSessionId;
    });
    expect(sessionIdBefore).toBeTruthy();
    await sidepanel.close();

    const panel2 = await context.newPage();
    await panel2.goto(`chrome-extension://${new URL(serviceWorker.url()).host}/sidepanel/sidepanel.html`);
    await expect(panel2.locator('#questionInput')).toBeVisible();

    // 本用例覆盖 chip 回填路径，test-menu-click 必须带 skipPanelOpen
    // （原因见 quote-collector.js：多面板实例竞争消费划词队列会覆盖会话状态）。

    await expect(panel2.locator('.msg.user').filter({ hasText: QUESTION })).toBeVisible();
    await expect(panel2.locator('.msg.assistant').filter({ hasText: 'mock 后端固定回复' })).toBeVisible();

    // 草稿回填：未发送文本 + 引用 chip（按 qid 从会话 quotes 快照还原）
    await expect(panel2.locator('#questionInput')).toContainText(DRAFT_TEXT);
    await expect(panel2.locator('#questionInput .quote-chip')).toHaveCount(1);

    await panel2.locator('#toggleHistory').click();
    await expect(panel2.locator('#historyDrawer')).toBeVisible();
    await expect(panel2.locator('.history-list li.active .h-title').filter({ hasText: QUESTION })).toBeVisible();
    const sessionIdAfter = await serviceWorker.evaluate(async () => {
      const c = (globalThis as { chrome: any }).chrome;
      return (await c.storage.local.get(['absorb-state']))['absorb-state'].currentSessionId;
    });
    expect(sessionIdAfter).toBe(sessionIdBefore);
  });
});
