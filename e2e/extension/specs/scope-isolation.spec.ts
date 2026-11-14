// P0-4：A/B 页面 scope 隔离（e2e.md 判定的最高价值用例）。
// 测试面板以普通标签页打开，点面板自己会成 active tab 导致 getActivePage 恒为 null；
// 因此提交统一走"文章页 bringToFront 置前 + 面板页 evaluate 里 element.click()"，
// 复现真实 Side Panel 场景（面板不是 tab，活动 tab 始终是网页）。
import { test, expect, FIXTURE_BASE, MOCK_PORT, closeTestPages, type Page, type BrowserContext, type Worker } from '../fixtures/extension.fixture';

const URL_A = `${FIXTURE_BASE}/article-a.html`;
const URL_B = `${FIXTURE_BASE}/article-b.html`;
const MARKER_A = 'absorb-fixture-alpha-marker';
const MARKER_B = 'absorb-fixture-beta-marker';
const FRAGMENT = '为大语言模型提供外部上下文';

type AnyChrome = { chrome: any };

async function openArticle(context: BrowserContext, url: string) {
  const page = await context.newPage();
  await page.goto(url);
  return page;
}

// 在页面里构造划词选区（Range API，与鼠标拖选等价）
async function selectFragment(page: Page) {
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
}

// 向面板输入问题。注意不能用 fill()：它重写 contenteditable 会清掉划词 chip 子元素，
// 必须以追加文本节点的方式输入（保留已有 chip，派发 input 触发 updateAskBtn）
async function typeInPanel(page: Page, question: string, keepChips = false) {
  if (keepChips) {
    await page.evaluate((q) => {
      const input = document.querySelector('#questionInput')!;
      input.appendChild(document.createTextNode(q));
      input.dispatchEvent(new InputEvent('input', { bubbles: true }));
    }, question);
  } else {
    await page.locator('#questionInput').fill(question);
  }
  await page.evaluate(() => (document.querySelector('#askBtn') as HTMLButtonElement).click());
}

// 轮询 /__last-ask 直到 question 匹配。注意 [[n]] 会被替换成引用文本前缀，用 includes
async function pollLastAsk(page: Page, question: string) {
  let payload: any;
  await expect.poll(async () => {
    payload = await page.evaluate(async (port) =>
      (await (await fetch(`http://127.0.0.1:${port}/__last-ask`)).json()).lastAsk, MOCK_PORT);
    return payload?.question?.includes(question);
  }, { timeout: 10_000 }).toBe(true);
  return payload;
}

async function askAndFinish(page: Page, question: string) {
  const before = await page.locator('.msg.assistant').count();
  await typeInPanel(page, question);
  await expect(page.locator('.msg.user').filter({ hasText: question })).toBeVisible();
  const payload = await pollLastAsk(page, question);
  // 等新 assistant 气泡出现且不再 streaming（先计数再判态，避免窗口期假通过）
  await expect(page.locator('.msg.assistant')).toHaveCount(before + 1);
  await expect(page.locator('.msg.assistant.streaming')).toHaveCount(0);
  return payload;
}

// 等待活动 tab 切换生效（bringToFront 后 active tab 状态有异步窗口，轮询确认）
async function expectActiveTab(serviceWorker: Worker, url: string) {
  await expect.poll(async () => {
    return serviceWorker.evaluate(async (u: string) => {
      const tabs = await (globalThis as { chrome: any }).chrome.tabs.query({ active: true, currentWindow: true });
      return tabs[0]?.url;
    }, url);
  }, { timeout: 5_000 }).toBe(url);
}

test.describe('A/B 页面 scope 隔离', () => {
  let sidepanel: Page;

  test.beforeEach(async ({ context, serviceWorker }) => {
    await serviceWorker.evaluate(async () => {
      await (globalThis as { chrome: any }).chrome.storage.local.clear();
    });
    await fetch(`http://127.0.0.1:${MOCK_PORT}/__ask-script`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(null), // 还原默认脚本（其他 spec 可能遗留定制脚本）
    });
    sidepanel = await context.newPage();
    await sidepanel.goto(`chrome-extension://${new URL(serviceWorker.url()).host}/sidepanel/sidepanel.html`);
    await expect(sidepanel.locator('#questionInput')).toBeVisible();
  });

  test.afterEach(async ({ context }) => {
    await closeTestPages(context);
  });

  test('引用锚定：A 页划词后切到 B 前台提问 → context 仍指 A，page 指向 B', async ({
    context, serviceWorker,
  }) => {
    // A 页划词采集（经 test-menu-click 钩子走真实链路）→ 面板出 chip
    const articleA = await openArticle(context, URL_A);
    await selectFragment(articleA);
    const tabIdA = await serviceWorker.evaluate(async (url: string) => {
      const tabs = await (globalThis as { chrome: any }).chrome.tabs.query({});
      return tabs.find((t: { url?: string }) => t.url === url)?.id;
    }, URL_A);
    await sidepanel.evaluate(async (id: number) => {
      await (globalThis as { chrome: any }).chrome.runtime.sendMessage({
        type: 'test-menu-click',
        skipPanelOpen: true, // E2E 不开原生面板
        info: { menuItemId: 'absorb-open-panel', selectionText: '为大语言模型提供外部上下文', frameId: 0 },
        tab: { id, url: 'x' },
      });
    }, tabIdA);
    await expect(sidepanel.locator('.quote-chip')).toHaveCount(1);

    // B 置为前台（真实场景：用户切到 B 页再看面板）
    const articleB = await openArticle(context, URL_B);
    await articleB.bringToFront();
    await expectActiveTab(serviceWorker, URL_B);

    await typeInPanel(sidepanel, '结合引用解释', true);
    const payload = await pollLastAsk(sidepanel, '结合引用解释');
    await expect(sidepanel.locator('.msg.assistant')).toBeVisible();

    // 引用材料仍锚定 A 页（scope 不因活动页切换漂移）；
    // net.js 只取第一条引用作为 context（单对象，非数组）
    expect(payload.context.url).toBe(URL_A);
    expect(payload.context.paragraph).toContain(FRAGMENT);
    expect(payload.context.pageMd).toContain(MARKER_A);
    // 页面归属指向当前在看的 B 页，整页材料是 B 的（A 不覆盖 B）
    expect(payload.page.url).toBe(URL_B);
    expect(payload.page.pageMd).toContain(MARKER_B);
    expect(payload.page.pageMd).not.toContain(MARKER_A);
  });

  test('页面归属实时切换：A/B 交替提问，page 各归其页无粘连', async ({ context, serviceWorker }) => {
    const articleA = await openArticle(context, URL_A);
    const articleB = await openArticle(context, URL_B);

    await articleA.bringToFront();
    await expectActiveTab(serviceWorker, URL_A);
    const p1 = await askAndFinish(sidepanel, '第一问');
    expect(p1.page.url).toBe(URL_A);
    expect(p1.page.pageMd).toContain(MARKER_A);

    await articleB.bringToFront();
    await expectActiveTab(serviceWorker, URL_B);
    const p2 = await askAndFinish(sidepanel, '第二问');
    expect(p2.page.url).toBe(URL_B);
    expect(p2.page.pageMd).toContain(MARKER_B);
    expect(p2.page.pageMd).not.toContain(MARKER_A);

    await articleA.bringToFront();
    await expectActiveTab(serviceWorker, URL_A);
    const p3 = await askAndFinish(sidepanel, '第三问');
    expect(p3.page.url).toBe(URL_A);
    expect(p3.page.pageMd).toContain(MARKER_A);
    expect(p3.page.pageMd).not.toContain(MARKER_B);
  });

  test('会话隔离：新会话不带 A 历史；流式中禁止开新会话（迟到回答守卫）', async ({ context, serviceWorker }) => {
    // 会话 A：完整问答
    const sessionA = await askAndFinish(sidepanel, 'A 会话的问题');
    await expect(sidepanel.locator('.msg.assistant').filter({ hasText: 'mock 后端固定回复' })).toBeVisible();

    // 开新会话问 B（B 页在前台，page 归属 B）
    await (await openArticle(context, URL_B)).bringToFront();
    await expectActiveTab(serviceWorker, URL_B);
    await sidepanel.evaluate(() => (document.querySelector('#newSession') as HTMLButtonElement).click());
    await askAndFinish(sidepanel, 'B 会话的问题');

    // 存储层：两个会话各自的消息互不渗漏
    const state = await serviceWorker.evaluate(async () => {
      const c = (globalThis as { chrome: any }).chrome;
      return await c.storage.local.get(['absorb-state']);
    });
    const st = state['absorb-state'];
    const sessions = st.sessions as Array<{ id: string; messages: Array<{ role: string; content: string }> }>;
    expect(sessions).toHaveLength(2);
    const sA = sessions.find((s) => s.messages.some((m) => m.content === 'A 会话的问题'))!;
    const sB = sessions.find((s) => s.messages.some((m) => m.content === 'B 会话的问题'))!;
    expect(sA.id).not.toBe(sB.id);
    expect(sA.messages.some((m) => m.content === 'B 会话的问题')).toBe(false);
    expect(sB.messages.some((m) => m.content === 'A 会话的问题')).toBe(false);
    expect(st.currentSessionId).toBe(sB.id);

    // 流式守卫：A 迟到回答场景的可达路径——流式中 #newSession 被拦截，
    // 会话不切换，迟到回答必然落回原线程。慢流脚本给 2.5s 流式窗口
    await fetch(`http://127.0.0.1:${MOCK_PORT}/__ask-script`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify([
        { type: 'session', sessionId: 'mock-session-slow' },
        { delayMs: 2500, data: { done: true, requestId: 'slow-1' } },
      ]),
    });
    const beforeSlow = await currentStorageSession(serviceWorker);
    await typeInPanel(sidepanel, '慢流问题');
    await expect(sidepanel.locator('.msg.assistant.streaming')).toBeVisible();
    await sidepanel.evaluate(() => (document.querySelector('#newSession') as HTMLButtonElement).click());
    expect(await currentStorageSession(serviceWorker)).toBe(beforeSlow);
    await expect(sidepanel.locator('.msg.assistant.streaming')).toBeVisible(); // 未被打断
    await expect(sidepanel.locator('.msg.assistant.streaming')).toHaveCount(0); // 自然完成
    const after = await serviceWorker.evaluate(async () => {
      const c = (globalThis as { chrome: any }).chrome;
      return await c.storage.local.get(['absorb-state']);
    });
    const sC = (after['absorb-state'].sessions as Array<{ id: string; messages: Array<{ role: string; content: string }> }>)
      .find((s) => s.messages.some((m) => m.content === '慢流问题'))!;
    expect(after['absorb-state'].currentSessionId).toBe(sC.id); // 回答落回发起会话
  });
});

async function currentStorageSession(serviceWorker: Worker): Promise<string | null> {
  const state = await serviceWorker.evaluate(async () => {
    const c = (globalThis as { chrome: any }).chrome;
    return await c.storage.local.get(['absorb-state']);
  });
  return state['absorb-state'].currentSessionId ?? null;
}
