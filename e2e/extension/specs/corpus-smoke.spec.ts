// P2：真实语料冒烟。docs/page-corpus 真实页面快照走扩展整链路
// （打开 → 划词 → test-menu-click 采集 → 提问），验证两件单测覆盖不到的事：
// 1) 真实复杂 DOM（巨型 HTML/SPA 壳/弹窗噪音）上链路可用；
// 2) pageMd 体积护栏（≤ 语料 meta 的 mdChars raw 基线）。
// 内容级回归已由 frontend/tests/pageconvert.spec.ts（Vitest）与
// tool/pageretriever/corpus_test.go（Go 同构）锁定——这里只断言 mustContain
// 词命中与预算上界，不断言精确文本（快照是历史样本，脆弱断言会误导维护）。
import fs from 'node:fs';
import path from 'node:path';
import { test, expect, FIXTURE_BASE, MOCK_PORT, closeTestPages, type Page, type Worker } from '../fixtures/extension.fixture';
import { selectNeedle } from '../fixtures/select-text';

const MOCK_BASE = `http://127.0.0.1:${MOCK_PORT}`;
const CORPUS_DIR = path.resolve(process.cwd(), '..', '..', 'docs', 'page-corpus');

// 每页一条冒烟：needle=划词目标（语料 query keywords 人工复核过的独特词），
// assertContains 取「扩展主内容提取后仍在正文」的词（语料 mustContain 是全 DOM
// 转换口径——如 cnblogs 的「赞助商」在侧栏，扩展提取不进来，不能直接搬用）。
// 体积口径注意：语料 mdChars 是「桌面端 Go TruncatePageText(30000) 截断后的入缓存值」，
// 而 ask payload 的 pageMd 是扩展端产出——上限是 grab.js MAX_PAGE_TEXT_CHARS(100K)，
// Go 截断发生在桌面缓存层。因此这里断言 payload ≤ 100K（扩展上限护栏）。
// spa-shell 是 CSR 壳页：无可选正文，走「无划词提问」验证壳页 pageMd 极薄、问答不受影响。
const MAX_PAGE_TEXT_CHARS = 100_000; // grab.js 扩展端 pageMd 上限
const CORPUS_PAGES = [
  { name: 'blog-cn/cnblogs', needle: 'Koog', assertContains: ['Koog'] },
  { name: 'weixin-gzh', needle: '武昌工学院', assertContains: ['武昌工学院'] },
  { name: 'spa-modal-noise', needle: '数据流动治理平台', assertContains: ['数据流动治理平台'], localOnly: true },
  { name: 'spa-shell', needle: '', assertContains: [], shell: true },
];

async function setAskScript(events: unknown[] | null) {
  const resp = await fetch(`${MOCK_BASE}/__ask-script`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(events),
  });
  expect(resp.ok).toBe(true);
}

// 慢速演示（ABSORB_E2E_SLOWMO>0）：固定回复拆成多段 token 延迟送出，便于观测流式过程；
// 常规跑返回 null（用 mock 桥默认脚本，最快）
const SLOWMO = Number(process.env.ABSORB_E2E_SLOWMO) || 0;
function demoAskScript(question: string) {
  if (!SLOWMO) return null;
  const tokens = ['E2E 回答：', `收到「${question}」`, '。这是', '多段流式', '输出演示，', '逐段送达。'];
  return [
    { type: 'session', sessionId: 'mock-session-1' },
    ...tokens.map((content) => ({ delayMs: 220, data: { type: 'token', content } })),
    { delayMs: 220, data: { done: true, requestId: 'mock-req-1' } },
  ];
}

test.describe('真实语料冒烟', () => {
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

  for (const p of CORPUS_PAGES) {
    test(`整链路冒烟：${p.name} → 划词 → 提问 → pageMd 命中与预算`, async ({ context, serviceWorker }) => {
      const sourcePath = path.join(CORPUS_DIR, p.name, 'source.html');
      if (p.localOnly && !fs.existsSync(sourcePath)) {
        test.skip(true, 'spa-modal-noise 为仅本地语料（不入库），缺失时跳过');
      }

      // 打开真实页面 →（有正文则）划词采集出 chip；壳页无正文直接提问
      const url = `${FIXTURE_BASE}/corpus/${p.name}/source.html`;
      const article = await context.newPage();
      // 语料快照含外网同步脚本（如 res.wx.qq.com）：外网不通时拖死 domcontentloaded，
      // 而快照只需要内联 DOM——非本地 http(s) 资源一律快速 abort，保证确定性
      await article.route(/^https?:\/\//, (route) => {
        return new URL(route.request().url()).hostname === '127.0.0.1'
          ? route.continue()
          : route.abort();
      });
      await article.goto(url, { waitUntil: 'domcontentloaded', timeout: 30_000 });
      if (!p.shell) {
        await selectNeedle(article, p.needle);
        const tabId = await serviceWorker.evaluate(async (u: string) => {
          const tabs = await (globalThis as { chrome: any }).chrome.tabs.query({});
          return tabs.find((t: { url?: string }) => t.url === u)?.id;
        }, url);
        await sidepanel.evaluate(async (id: number) => {
          await (globalThis as { chrome: any }).chrome.runtime.sendMessage({
            type: 'test-menu-click',
            skipPanelOpen: true, // E2E 不开原生面板
            info: { menuItemId: 'absorb-open-panel', selectionText: '语料划词', frameId: 0 },
            tab: { id, url: 'x' },
          });
        }, tabId);
        await expect(sidepanel.locator('.quote-chip')).toHaveCount(1);
      }

      // 页面置前台（整页材料归属该页）→ 提问
      await article.bringToFront();
      await expect.poll(async () => serviceWorker.evaluate(async (u: string) => {
        const tabs = await (globalThis as { chrome: any }).chrome.tabs.query({ active: true, currentWindow: true });
        return tabs[0]?.url;
      }, url), { timeout: 5_000 }).toBe(url);

      const question = `语料冒烟：${p.name}`;
      if (SLOWMO) await setAskScript(demoAskScript(question));
      await sidepanel.locator('#questionInput').fill(question);
      await sidepanel.locator('#askBtn').click();

      // payload 断言：page 归属正确、mustContain 命中、体积不超 mdChars raw 基线
      let payload: any;
      await expect.poll(async () => {
        payload = await sidepanel.evaluate(async (port) =>
          (await (await fetch(`http://127.0.0.1:${port}/__last-ask`)).json()).lastAsk, MOCK_PORT);
        return payload?.question?.includes(question);
      }, { timeout: 15_000 }).toBe(true);
      expect(payload.page.url).toBe(url);
      const pageMd = String(payload.page.pageMd || '');
      if (p.shell) {
        // 壳页边界：转换产出极薄（README 口径 ≤100 字符），提问链路不受影响
        expect(pageMd.length, '壳页 pageMd 应极薄').toBeLessThanOrEqual(100);
      } else {
        // 扩展端上限护栏（grab.js MAX_PAGE_TEXT_CHARS；Go 30000 截断在桌面缓存层）
        expect(pageMd.length, 'pageMd 不应超扩展端上限').toBeLessThanOrEqual(MAX_PAGE_TEXT_CHARS);
        expect(pageMd.length, 'pageMd 非空').toBeGreaterThan(0);
      }
      for (const word of p.assertContains) {
        expect(pageMd).toContain(word);
      }
      await expect(sidepanel.locator('.msg.assistant')).toBeVisible();
      await expect(sidepanel.locator('.msg.assistant.streaming')).toHaveCount(0);

      // 第二轮：同会话追问（多轮对话）——历史区出现第二条 assistant 气泡
      const followUp = `追问：${p.name} 的要点是什么？`;
      if (SLOWMO) await setAskScript(demoAskScript(followUp));
      await sidepanel.locator('#questionInput').fill(followUp);
      await sidepanel.locator('#askBtn').click();
      await expect.poll(async () => {
        const latest = await sidepanel.evaluate(async (port) =>
          (await (await fetch(`http://127.0.0.1:${port}/__last-ask`)).json()).lastAsk, MOCK_PORT);
        return latest?.question;
      }, { timeout: 15_000 }).toBe(followUp);
      // page 归属保持该语料页（同会话内 page 上下文不漂移）
      expect(payload.page.url).toBe(url);
      await expect(sidepanel.locator('.msg.assistant')).toHaveCount(2);
      await expect(sidepanel.locator('.msg.assistant.streaming')).toHaveCount(0);
    });
  }
});
