// 扩展 E2E fixture：每个 worker（spec 文件）启动一个带扩展的独立 Chromium profile，
// 关闭即销毁临时目录。mock 桥端口经 storage.session 注入（background.js 测试钩子 A）。
import {
  test as base,
  chromium,
  type BrowserContext,
  type Worker,
} from '@playwright/test';
import path from 'node:path';
import os from 'node:os';
import fs from 'node:fs/promises';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));

/** 扩展源码目录（纯静态 JS，无构建步骤，直接加载） */
export const EXTENSION_PATH = path.resolve(here, '..', '..', '..', 'extension');
/** mock 桥端口（见 fixtures/servers.mjs），与 SW 测试钩子 testBridgePort 对应 */
export const MOCK_PORT = 8877;
export const MOCK_BASE = `http://127.0.0.1:${MOCK_PORT}`;
/** fixture 网页服务地址 */
export const FIXTURE_BASE = 'http://127.0.0.1:8899';
/** 哨兵页：常驻保底页面，防止 afterEach 清页后窗口归零 */
export const SENTINEL_URL = `${FIXTURE_BASE}/index.html`;

// 有头模式：清空全部页面会关掉最后一个窗口，Chrome 随即终止扩展 SW（浏览器进程
// 因 Playwright 禁用 DestroyProfileOnBrowserClose 而存活，下个用例再唤醒——表现为
// fixture 装配偶发 target closed）。无头无窗口不受影响。哨兵页让窗口永不为空。
export async function closeTestPages(context: BrowserContext) {
  for (const p of context.pages()) {
    if (p.url() === SENTINEL_URL) continue;
    await p.close().catch(() => {});
  }
}

type Fixtures = {
  /** worker 级共享 context：同 spec 文件内用例共用一个 profile */
  extensionContext: BrowserContext;
  /** 兼容 Playwright 惯用名的别名 */
  context: BrowserContext;
  /** 扩展 MV3 Service Worker */
  serviceWorker: Worker;
  /** 扩展 ID（manifest 含 key，固定可预测） */
  extensionId: string;
};

export const test = base.extend<Fixtures>({
  extensionContext: [
    async ({}, use) => {
      const userDataDir = await fs.mkdtemp(path.join(os.tmpdir(), 'absorb-e2e-'));
      const context = await chromium.launchPersistentContext(userDataDir, {
        channel: 'chromium',
        // 默认无头（新版 headless 支持扩展加载）；本地调试可设 ABSORB_E2E_HEADED=1
        headless: !process.env.ABSORB_E2E_HEADED,
        // 慢速演示：ABSORB_E2E_SLOWMO=毫秒，给每条 CDP 命令加延迟（配合 HEADED 观测用）
        slowMo: Number(process.env.ABSORB_E2E_SLOWMO) || 0,
        // 固定中文 locale：与生产 default_locale 一致，UI 文案断言不随环境语言漂移
        locale: 'zh-CN',
        args: [
          `--disable-extensions-except=${EXTENSION_PATH}`,
          `--load-extension=${EXTENSION_PATH}`,
        ],
      });
      // 哨兵页：任何时刻至少保留一个页面，窗口不归零（见 closeTestPages 注释）
      const sentinel = await context.newPage();
      await sentinel.goto(SENTINEL_URL);
      try {
        await use(context);
      } finally {
        await context.close();
        await fs.rm(userDataDir, { recursive: true, force: true }).catch(() => {});
      }
    },
    { scope: 'worker', timeout: 0 },
  ],

  context: async ({ extensionContext }, use) => use(extensionContext),

  serviceWorker: async ({ context }, use) => {
    // MV3 SW 会被 Chrome 回收后自动重启（有头模式关全部页面时更易触发；
    // 无头从不回收所以不复现）。serviceWorkers() 可能短暂残留已终止的旧实例，
    // 盲取 [0] 会拿到死目标——从最新往回探活，取第一个活着的
    const pick = async () => {
      const list = context.serviceWorkers();
      for (let i = list.length - 1; i >= 0; i--) {
        try {
          await list[i].evaluate(() => 1);
          return list[i];
        } catch { /* 已终止，继续找 */ }
      }
      return null;
    };
    let worker = await pick();
    if (!worker) worker = await context.waitForEvent('serviceworker');
    // 测试钩子 A：注入 mock 桥端口 + 宿主安装状态，get-bridge-port 将直返二者。
    // testHostStatus 缺省 installed（测试环境宿主视作在册）；未安装引导用例自行覆盖
    await worker.evaluate(
      (port) => (globalThis as { chrome: { storage: { session: { set: (o: object) => Promise<void> } } } })
        .chrome.storage.session.set({ testBridgePort: port, testHostStatus: 'installed' }),
      MOCK_PORT,
    );
    await use(worker);
  },

  extensionId: async ({ serviceWorker }, use) => {
    await use(new URL(serviceWorker.url()).host);
  },
});

export const expect = base.expect;
