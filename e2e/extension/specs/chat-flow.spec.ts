// P0-3：SSE 问答。输入 → 发送 → 流式渲染 → 会话持久化；以及主动停止生成。
// SSE 协议以 net.js 解析器为准，事件序列经 /__ask-script 定制或用默认脚本。
import { test, expect, MOCK_PORT, closeTestPages, type Page, type BrowserContext, type Worker } from '../fixtures/extension.fixture';

const MOCK_BASE = `http://127.0.0.1:${MOCK_PORT}`;
const QUESTION = '什么是 RAG？';

// 向 mock 桥写入 SSE 事件脚本（node 侧 fetch）
async function setAskScript(events: unknown[] | null) {
  const resp = await fetch(`${MOCK_BASE}/__ask-script`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(events),
  });
  expect(resp.ok).toBe(true);
}

async function openSidepanel(context: BrowserContext, serviceWorker: Worker) {
  const page = await context.newPage();
  await page.goto(`chrome-extension://${new URL(serviceWorker.url()).host}/sidepanel/sidepanel.html`);
  await expect(page.locator('#questionInput')).toBeVisible();
  return page;
}

// 输入问题并点击发送（#questionInput 为 contenteditable，fill 支持并派发 input 事件）
async function ask(page: Page, question: string) {
  await page.locator('#questionInput').fill(question);
  const askBtn = page.locator('#askBtn');
  await expect(askBtn).toBeEnabled();
  await askBtn.click();
}

test.describe('SSE 问答', () => {
  let sidepanel: Page;

  test.beforeEach(async ({ context, serviceWorker }) => {
    await serviceWorker.evaluate(async () => {
      await (globalThis as { chrome: any }).chrome.storage.local.clear();
    });
    await setAskScript(null); // 还原默认脚本
    sidepanel = await openSidepanel(context, serviceWorker);
  });

  test.afterEach(async ({ context }) => {
    await setAskScript(null);
    await closeTestPages(context);
  });

  test('流式问答全链路：用户气泡 → token 流渲染 → 结束态 → 会话持久化', async ({ serviceWorker }) => {
    // 每个事件间隔 200ms，让流式窗口可观测（默认脚本瞬间写完，.streaming 断言会竞态）
    await setAskScript([
      { type: 'session', sessionId: 'mock-session-1' },
      { delayMs: 200, data: { type: 'token', content: 'E2E 回答第一段。' } },
      { delayMs: 200, data: { type: 'token', content: '第二段：mock 后端固定回复' } },
      { delayMs: 200, data: { done: true, requestId: 'mock-req-1' } },
    ]);

    await ask(sidepanel, QUESTION);

    // 用户气泡立即出现
    await expect(sidepanel.locator('.msg.user').filter({ hasText: QUESTION })).toBeVisible();

    // 流式期间：assistant 气泡带 .streaming，按钮切"停止"
    await expect(sidepanel.locator('.msg.assistant')).toBeVisible();
    await expect(sidepanel.locator('.msg.assistant.streaming')).toBeVisible();
    await expect(sidepanel.locator('#askBtn')).toHaveText('停止');

    // token 流完整渲染（默认脚本两段 token），流结束后 .streaming 移除、按钮复原
    await expect(
      sidepanel.locator('.msg.assistant').filter({ hasText: 'mock 后端固定回复' }),
    ).toBeVisible();
    await expect(sidepanel.locator('.msg.assistant.streaming')).toHaveCount(0);
    await expect(sidepanel.locator('#askBtn')).toHaveText('发送');

    // 提交 payload 断言：问题与 sessionId 原样上送
    const lastAsk = await sidepanel.evaluate(async (base) => {
      return await (await fetch(`${base}/__last-ask`)).json();
    }, MOCK_BASE);
    expect(lastAsk.lastAsk.question).toBe(QUESTION);

    // 会话持久化：currentSessionId 存在且绑定了 mock 下发的 sessionId
    const state = await serviceWorker.evaluate(async () => {
      const c = (globalThis as { chrome: any }).chrome;
      return await c.storage.local.get(['absorb-state']);
    });
    const st = state['absorb-state'];
    expect(st.currentSessionId).toBeTruthy();
    const session = st.sessions.find((s: { id: string }) => s.id === st.currentSessionId);
    expect(session.sessionId).toBe('mock-session-1');
    expect(session.messages.some((m: { role: string; content: string }) =>
      m.role === 'assistant' && m.content.includes('mock 后端固定回复'))).toBe(true);
  });

  test('停止生成：流式中点击停止 → 保留半截回答 + "已停止"标记', async () => {
    await setAskScript([
      { type: 'session', sessionId: 'mock-session-2' },
      { type: 'token', content: '前半段回答' },
      { delayMs: 30_000, data: { done: true, requestId: 'never' } }, // 卡住模拟长回答
    ]);

    await ask(sidepanel, '长问题');
    await expect(sidepanel.locator('.msg.assistant.streaming')).toBeVisible();
    await expect(sidepanel.locator('#askBtn')).toHaveText('停止');

    await sidepanel.locator('#askBtn').click();

    // 停止后：半截回答保留 + .stop-mark 标记，按钮复原
    await expect(sidepanel.locator('.msg.assistant').filter({ hasText: '前半段回答' })).toBeVisible();
    await expect(sidepanel.locator('.stop-mark')).toHaveText('⏹ 已停止');
    await expect(sidepanel.locator('.msg.assistant.streaming')).toHaveCount(0);
    await expect(sidepanel.locator('#askBtn')).toHaveText('发送');
  });
});
