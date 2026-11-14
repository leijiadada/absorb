// P0-1：扩展加载与启动。验证真实 Chromium 能装下 Absorb、SW 存活、
// Side Panel 页面渲染、端口测试钩子生效、扩展页面可达 mock 桥。
import { test, expect, MOCK_PORT, MOCK_BASE } from '../fixtures/extension.fixture';

test.describe('扩展加载与启动', () => {
  test('扩展成功加载且 Service Worker 运行中', async ({ extensionId, serviceWorker }) => {
    // manifest 含 key → 扩展 ID 固定为 32 个 a-p 字符
    expect(extensionId).toMatch(/^[a-p]{32}$/);
    // SW 可执行代码：确认 worker 存活
    expect(await serviceWorker.evaluate(() => 1 + 1)).toBe(2);
  });

  test('Side Panel 页面可加载且输入区可用', async ({ context, extensionId }) => {
    const page = await context.newPage();
    await page.goto(`chrome-extension://${extensionId}/sidepanel/sidepanel.html`);
    await expect(page.locator('#questionInput')).toBeVisible();
    await expect(page.locator('#messages')).toBeVisible();
    await expect(page.locator('#askBtn')).toBeDisabled();
  });

  test('端口测试钩子生效：get-bridge-port 返回 mock 桥端口', async ({ context, extensionId }) => {
    const page = await context.newPage();
    await page.goto(`chrome-extension://${extensionId}/sidepanel/sidepanel.html`);
    const port = await page.evaluate(async () => {
      const resp = await (globalThis as {
        chrome: { runtime: { sendMessage: (m: object) => Promise<{ port?: number }> } };
      }).chrome.runtime.sendMessage({ type: 'get-bridge-port' });
      return resp?.port;
    });
    expect(port).toBe(MOCK_PORT);
  });

  test('扩展页面可访问 mock 桥（host_permissions + mock 服务在线）', async ({ context, extensionId }) => {
    const page = await context.newPage();
    await page.goto(`chrome-extension://${extensionId}/sidepanel/sidepanel.html`);
    const health = await page.evaluate(async (base) => {
      const resp = await fetch(`${base}/health`);
      return resp.json();
    }, MOCK_BASE);
    expect(health.ok).toBe(true);
  });
});
