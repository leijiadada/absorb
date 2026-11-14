// 桌面端未安装引导：宿主注册缺失（testHostStatus='not-installed'）时空态区显示
// 下载引导卡片，提问错误文案指向"未安装"；重新检测到已安装后卡片消失；
// 已安装（fixture 默认注入 installed）时全程无卡片。
// 注：真实 connectNative 分类（native-connection.js）不在 E2E 范围（测试钩子绕开管道）。
import { test, expect, MOCK_PORT, closeTestPages, type BrowserContext, type Worker } from '../fixtures/extension.fixture';

const DOWNLOAD_URL = 'https://gitee.com/leij1/absorb/releases';

// 覆盖 SW 测试钩子（fixture 默认 testBridgePort=MOCK_PORT + installed）
async function setHostMock(worker: Worker, hostStatus: string, port = 0) {
  await worker.evaluate(
    ({ hostStatus, port }: { hostStatus: string; port: number }) =>
      (globalThis as { chrome: { storage: { session: { set: (o: object) => Promise<void> } } } })
        .chrome.storage.session.set({ testBridgePort: port, testHostStatus: hostStatus }),
    { hostStatus, port },
  );
}

async function openPanel(context: BrowserContext, serviceWorker: Worker) {
  const page = await context.newPage();
  await page.goto(`chrome-extension://${new URL(serviceWorker.url()).host}/sidepanel/sidepanel.html`);
  await expect(page.locator('#questionInput')).toBeVisible();
  return page;
}

test.describe('桌面端未安装引导', () => {
  let page: Awaited<ReturnType<typeof openPanel>>;

  test.beforeEach(async ({ context, serviceWorker }) => {
    await serviceWorker.evaluate(async () => {
      const c = (globalThis as { chrome: any }).chrome;
      await c.storage.local.clear();
    });
    page = await openPanel(context, serviceWorker);
  });

  test.afterEach(async ({ context }) => {
    await page.close().catch(() => {});
    await closeTestPages(context);
  });

  test('未安装 → 空态区显示下载卡片', async ({ serviceWorker }) => {
    await setHostMock(serviceWorker, 'not-installed');
    // beforeEach 的面板按 fixture 默认 installed 打开，reload 让 init 按新 mock 重新探测
    await page.reload();
    const card = page.locator('#desktopGuide');
    await expect(card).toBeVisible();
    await expect(card.locator('.dg-title')).toHaveText('需要 Absorb 桌面端');
    await expect(page.locator('#dgDownload')).toBeVisible();
    await expect(page.locator('#dgRecheck')).toBeVisible();
  });

  test('重新检测到已安装 → 卡片消失', async ({ serviceWorker }) => {
    await setHostMock(serviceWorker, 'not-installed');
    await page.reload();
    await expect(page.locator('#desktopGuide')).toBeVisible();
    // 用户装完桌面端：恢复 mock 端口 + installed，点"重新检测"
    await setHostMock(serviceWorker, 'installed', MOCK_PORT);
    await page.locator('#dgRecheck').click();
    await expect(page.locator('#desktopGuide')).toBeHidden();
  });

  test('已安装（运行或未运行）→ 不显示卡片', async ({ serviceWorker }) => {
    // 运行中（端口>0）与已装未运行（端口0）都视作已安装：卡片均不出现
    await setHostMock(serviceWorker, 'installed', MOCK_PORT);
    await page.reload();
    await expect(page.locator('#desktopGuide')).toBeHidden();

    await setHostMock(serviceWorker, 'installed', 0);
    await page.reload();
    await expect(page.locator('#desktopGuide')).toBeHidden();
  });

  test('未安装时提问 → 错误文案指向未安装而非未启动', async ({ serviceWorker }) => {
    await setHostMock(serviceWorker, 'not-installed');
    await page.reload();
    await page.locator('#questionInput').fill('这个问题发不出去');
    await page.locator('#askBtn').click();
    await expect(page.locator('.msg.assistant').filter({ hasText: '尚未安装 Absorb 桌面端' })).toBeVisible();
    // 未安装不是"服务未启动"：不显示重连条（重连条只对连接类错误出现）
    await expect(page.locator('#reconnectBar')).toBeHidden();
  });

  test('下载按钮 → 新开 releases 落地页', async ({ serviceWorker, context }) => {
    await setHostMock(serviceWorker, 'not-installed');
    await page.reload();
    await expect(page.locator('#desktopGuide')).toBeVisible();
    const popupPromise = context.waitForEvent('page');
    await page.locator('#dgDownload').click();
    const popup = await popupPromise;
    expect(popup.url()).toBe(DOWNLOAD_URL);
    await popup.close().catch(() => {});
  });
});
