// P1-2：SW 重启。模拟 MV3 Service Worker 被 Chrome 回收（CDP Target.closeTarget 强停）
// 后由事件唤醒重启。验证三件事：storage.local 会话状态跨重启存活、storage.session 的
// testBridgePort 钩子仍生效（get-bridge-port 直返 mock 端口）、重启后问答链路端到端可用。
// 注：真实 Native 管道重连不在 E2E 范围（测试钩子 A 本就绕开管道），由 Vitest 覆盖。
import { test, expect, MOCK_PORT, closeTestPages, type Page, type BrowserContext, type Worker } from '../fixtures/extension.fixture';

const Q1 = '重启前的问题';
const Q2 = '重启后的问题';

async function setAskScript(sessionId: string, requestId: string) {
  await fetch(`http://127.0.0.1:${MOCK_PORT}/__ask-script`, {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify([
      { type: 'session', sessionId },
      { type: 'token', content: `E2E 回答：收到「${sessionId}」` },
      { done: true, requestId },
    ]),
  });
}

async function openPanel(context: BrowserContext, serviceWorker: Worker) {
  const page = await context.newPage();
  await page.goto(`chrome-extension://${new URL(serviceWorker.url()).host}/sidepanel/sidepanel.html`);
  await expect(page.locator('#questionInput')).toBeVisible();
  return page;
}

async function askAndFinish(page: Page, question: string) {
  await page.locator('#questionInput').fill(question);
  await page.locator('#askBtn').click();
  await expect(page.locator('.msg.user').filter({ hasText: question })).toBeVisible();
  await expect(page.locator('.msg.assistant.streaming')).toHaveCount(0);
}

async function currentSessionId(worker: Worker) {
  return worker.evaluate(async () => {
    const c = (globalThis as { chrome: any }).chrome;
    return (await c.storage.local.get(['absorb-state']))['absorb-state'].currentSessionId;
  });
}

test.describe('SW 重启', () => {
  test.beforeEach(async ({ context, serviceWorker }) => {
    await serviceWorker.evaluate(async () => {
      await (globalThis as { chrome: any }).chrome.storage.local.clear();
    });
    await fetch(`http://127.0.0.1:${MOCK_PORT}/__ask-script`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(null),
    });
  });

  test.afterEach(async ({ context }) => {
    await fetch(`http://127.0.0.1:${MOCK_PORT}/__ask-script`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(null),
    });
    await closeTestPages(context);
  });

  test('SW 被强停后重启 → 状态存活、端口钩子生效、问答链路恢复', async ({ context, serviceWorker }) => {
    // 重启前：完整问答一轮（会话落盘）
    await setAskScript('mock-session-sw', 'sw-req-1');
    const panel1 = await openPanel(context, serviceWorker);
    await askAndFinish(panel1, Q1);
    const sessionIdBefore = await currentSessionId(serviceWorker);
    expect(sessionIdBefore).toBeTruthy();

    // CDP 强停 SW（模拟 MV3 空闲回收/崩溃），确认旧实例已死。
    // newCDPSession 不接受 Worker，改挂页面会话，Target 域命令路由到浏览器端
    const cdp = await context.newCDPSession(panel1);
    const { targetInfos } = await cdp.send('Target.getTargets');
    const swTarget = targetInfos.find(
      (t) => t.type === 'service_worker' && t.url.includes('background'),
    );
    expect(swTarget, '应能找到扩展 SW target').toBeTruthy();
    await cdp.send('Target.closeTarget', { targetId: swTarget!.targetId });
    await expect.poll(async () => {
      try {
        await serviceWorker.evaluate(() => 1);
        return false;
      } catch {
        return true; // 旧实例已终止
      }
    }, { timeout: 10_000 }).toBe(true);

    // 重开面板：init 的 get-bridge-port 消息经 runtime.onMessage 唤醒 SW 重启。
    // Chrome 只为「已注册监听器的事件」唤醒 SW（background 不监听 tabs 事件，
    // 开网页 tab 唤不醒），运行时消息正是产品的真实唤醒路径
    const panel2 = await openPanel(context, serviceWorker);
    let sw2: Worker | null = null;
    await expect.poll(async () => {
      const list = context.serviceWorkers();
      for (let i = list.length - 1; i >= 0; i--) {
        try {
          await list[i].evaluate(() => 1);
          sw2 = list[i];
          return true; // 旧实例已死，活着的必是新实例
        } catch { /* 已终止，继续找 */ }
      }
      return false;
    }, { timeout: 15_000 }).toBe(true);
    expect(new URL(sw2!.url()).host).toBe(new URL(serviceWorker.url()).host); // 同一扩展的新实例

    // storage.session 的 testBridgePort 跨 SW 重启存活 → 端口钩子仍生效
    // （从页面上下文 sendMessage，SW 不接收自己发出的 runtime.sendMessage）
    const port = await panel2.evaluate(async () => {
      const resp = await (globalThis as { chrome: any }).chrome.runtime.sendMessage({ type: 'get-bridge-port' });
      return resp?.port;
    });
    expect(port).toBe(MOCK_PORT);

    expect(await currentSessionId(sw2)).toBe(sessionIdBefore);

    // 重启后问答链路端到端可用（还原默认脚本，走完整 echo 回路）
    await fetch(`http://127.0.0.1:${MOCK_PORT}/__ask-script`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(null),
    });
    await askAndFinish(panel2, Q2);
    await expect(panel2.locator('.msg.assistant').filter({ hasText: Q2 })).toBeVisible();

    await expect(panel2.locator('.msg.user').filter({ hasText: Q1 })).toBeVisible();

    // mock 桥确实收到了重启后的提问（新 SW → 端口钩子 → mock 桥全链路）
    await expect.poll(async () => {
      const last = await panel2.evaluate(async (p) =>
        (await (await fetch(`http://127.0.0.1:${p}/__last-ask`)).json()).lastAsk, MOCK_PORT);
      return last?.question?.includes(Q2);
    }, { timeout: 10_000 }).toBe(true);
  });
});
