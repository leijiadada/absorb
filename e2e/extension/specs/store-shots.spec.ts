// 商店截图产出（非回归用例）：面板 420×800 + 语料文章页 860×800 分别截图，
// raw 输出 docs/store/shots/raw，由 scripts/store-shots-compose.ps1 合成为 1280×800 商店图
// 与 440×280 promo 瓷砖。默认跳过，跑法：
//   $env:ABSORB_E2E_STORE_SHOTS='1'; npx playwright test specs/store-shots.spec.ts
//   powershell -NoProfile -ExecutionPolicy Bypass -File scripts/store-shots-compose.ps1
// 三条演示路径：划词提问 / 多轮追问（表格+mermaid）/ 知识保存。默认跳过不进常规套件。
import { test, expect, MOCK_PORT, FIXTURE_BASE, closeTestPages, type Page, type Worker } from '../fixtures/extension.fixture';
import { selectNeedle } from '../fixtures/select-text';
import path from 'node:path';
import fs from 'node:fs/promises';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const RAW_DIR = path.resolve(here, '..', '..', '..', 'docs', 'store', 'shots', 'raw');
const ARTICLE_URL = `${FIXTURE_BASE}/store-article.html`;
const PROMO_URL = `${FIXTURE_BASE}/store-promo.html`;

// 划词 chip 文案（取自 store-article.html 正文金句，与左侧页面呼应）
const QUOTE = '迁移成本不在工具，在肌肉记忆';

// 三个问题全仓唯一（skill 约束：pollLastAsk 按 includes 匹配，禁止跨 spec 撞车）
const Q1 = '从 Cursor 迁到 Codex 会不习惯吗？';
const Q2 = '把主要差异整理成对比表，并画出迁移决策流程';

const ANSWER_1 = [
  '**结论先行：迁移可行，但要给自己留两周适应期。**',
  '',
  '### 真实差异',
  '1. **补全风格**：Codex 更克制，主动补全频率低于 Cursor，长函数要自己起头',
  '2. **上下文策略**：Codex 的 @ 引用更精确，但跨文件联想弱一档',
  '3. **计费方式**：按量计费比订阅制便宜，重度使用者需自行评估',
  '',
  '### 建议的迁移节奏',
  '- 第 1 周：并行使用，逐项替换肌肉记忆',
  '- 第 2 周：全量切换，记录别扭的点回头查配置',
  '',
  '```bash',
  '# 保存 Cursor 常用规则，导入 Codex',
  'cp .cursorrules ~/.codex/instructions.md',
  '```',
  '',
  '> 作者结论：两周后回不去了，补全质量是主要拉力。',
].join('\n');

const ANSWER_2 = [
  '**三类差异一张表，决策流程看下图。**',
  '',
  '| 维度 | Cursor | Codex |',
  '|---|---|---|',
  '| 补全频率 | 高频主动 | 克制按需 |',
  '| 上下文引用 | 联想面广 | @ 精确指向 |',
  '| 计费 | 订阅制 | 按量计费 |',
  '| 迁移成本 | — | 约 2 周 |',
  '',
  '```mermaid',
  'flowchart LR',
  '  A[重度依赖自动补全] --> Z[留在 Cursor 订阅]',
  '  B[高频引用大仓库] --> Y[切 Codex 按量计费]',
  '  C[两者都用] -->|并行两周| Y',
  '```',
  '',
  '> 决策原则：按"补全依赖度 × 仓库规模"两个维度选，不要只看价格。',
].join('\n');

type AskScript = unknown[];

function richScript(sessionId: string, requestId: string, answer: string): AskScript {
  // 拆成 3 段模拟流式；最终渲染在 done 后统一解析 Markdown
  const third = Math.ceil(answer.length / 3);
  return [
    { type: 'session', sessionId },
    { delayMs: 60, data: { type: 'token', content: answer.slice(0, third) } },
    { delayMs: 60, data: { type: 'token', content: answer.slice(third, third * 2) } },
    { delayMs: 60, data: { type: 'token', content: answer.slice(third * 2) } },
    { delayMs: 60, data: { done: true, requestId } },
  ];
}

async function setAskScript(events: AskScript | null) {
  const resp = await fetch(`http://127.0.0.1:${MOCK_PORT}/__ask-script`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(events),
  });
  expect(resp.ok).toBe(true);
}

// 文章页置前台并确认（skill：bringToFront 有异步窗口，必须轮询 tabs.query 确认）
async function activateArticle(serviceWorker: Worker, url: string) {
  await articleToFront(serviceWorker, url);
  await expect.poll(async () => serviceWorker.evaluate(async (u: string) => {
    const tabs = await (globalThis as { chrome: any }).chrome.tabs.query({ active: true, currentWindow: true });
    return tabs[0]?.url;
  }, url), { timeout: 10_000 }).toBe(url);
}

async function articleToFront(serviceWorker: Worker, url: string) {
  const tabId = await serviceWorker.evaluate(async (u: string) => {
    const tabs = await (globalThis as { chrome: any }).chrome.tabs.query({});
    return tabs.find((t: { url?: string }) => t.url === u)?.id;
  }, url);
  await serviceWorker.evaluate(async (id: number) => {
    await (globalThis as { chrome: any }).chrome.tabs.update(id, { active: true });
  }, tabId);
}

async function ask(panel: Page, question: string) {
  // 无 chip 时 fill 安全；有 chip 的场景必须走追加文本节点（fill 会清掉 chip）
  await panel.evaluate((q: string) => {
    const input = document.querySelector('#questionInput')!;
    input.appendChild(document.createTextNode(q));
    input.dispatchEvent(new InputEvent('input', { bubbles: true }));
  }, question);
  await panel.locator('#askBtn').click();
}

// 面板消息区滚到底：done 后 mermaid/表格渲染会撑高内容，不滚则图在视口外
async function scrollPanelBottom(panel: Page) {
  await panel.evaluate(() => {
    const el = document.querySelector('#messages') as HTMLElement | null;
    if (el) el.scrollTop = el.scrollHeight;
  });
  await panel.waitForTimeout(150);
}

test.describe('商店截图产出', () => {
  test.skip(!process.env.ABSORB_E2E_STORE_SHOTS, '设 ABSORB_E2E_STORE_SHOTS=1 才跑（产出 docs/store/shots/raw 截图，非回归用例）');
  test.setTimeout(300_000);

  let panel: Page;
  let article: Page;

  test.beforeEach(async ({ context, serviceWorker }) => {
    await serviceWorker.evaluate(async () => {
      await (globalThis as { chrome: any }).chrome.storage.local.clear();
    });
    await setAskScript(null);
    await fs.mkdir(RAW_DIR, { recursive: true });
    panel = await context.newPage();
    await panel.setViewportSize({ width: 420, height: 800 });
    await panel.goto(`chrome-extension://${new URL(serviceWorker.url()).host}/sidepanel/sidepanel.html`);
    await expect(panel.locator('#questionInput')).toBeVisible();
  });

  test.afterEach(async ({ context }) => {
    await setAskScript(null);
    await closeTestPages(context);
  });

  test('三条演示路径：划词提问 / 多轮追问 / 知识保存 + promo 瓷砖', async ({ context, serviceWorker }) => {
    // ---- 文章页（左半 860×800）：掘金快照，外链 abort（语料页含外网同步脚本会拖死加载） ----
    article = await context.newPage();
    await article.setViewportSize({ width: 860, height: 800 });
    await article.route(/^https?:\/\//, (route) => {
      const u = route.request().url();
      return u.startsWith(FIXTURE_BASE) ? route.continue() : route.abort();
    });
    await article.goto(ARTICLE_URL, { waitUntil: 'domcontentloaded', timeout: 60_000 });
    await expect(article.locator('body')).toBeVisible();

    // ---- Shot 1：划词提问（chip + 引用回答 + 代码高亮） ----
    // 划选整句金句（chip 文案与保存面板"原文摘录"一致）
    await selectNeedle(article, QUOTE);
    const tabId = await serviceWorker.evaluate(async (u: string) => {
      const tabs = await (globalThis as { chrome: any }).chrome.tabs.query({});
      return tabs.find((t: { url?: string }) => t.url === u)?.id;
    }, ARTICLE_URL);
    await panel.evaluate(async ([id, quote]: [number, string]) => {
      await (globalThis as { chrome: any }).chrome.runtime.sendMessage({
        type: 'test-menu-click',
        skipPanelOpen: true,
        info: { menuItemId: 'absorb-open-panel', selectionText: quote, frameId: 0 },
        tab: { id, url: 'x' },
      });
    }, [tabId, QUOTE]);
    await expect(panel.locator('.quote-chip')).toHaveCount(1);

    await setAskScript(richScript('shot-s1', 'shot-req-1', ANSWER_1));
    await ask(panel, Q1);
    await expect.poll(async () => {
      const payload = await panel.evaluate(async (port) =>
        (await (await fetch(`http://127.0.0.1:${port}/__last-ask`)).json()).lastAsk, MOCK_PORT);
      return String(payload?.question ?? '').includes(Q1);
    }, { timeout: 20_000 }).toBe(true);
    await expect(panel.locator('.msg.assistant')).toBeVisible();
    await expect(panel.locator('.msg.assistant.streaming')).toHaveCount(0);
    await expect(panel.locator('.msg.assistant pre').first()).toBeVisible();
    await scrollPanelBottom(panel);
    await panel.screenshot({ path: path.join(RAW_DIR, 'panel-1.png') });
    await article.screenshot({ path: path.join(RAW_DIR, 'article-1.png') });

    // ---- Shot 2：多轮追问（表格 + mermaid 流程图） ----
    await activateArticle(serviceWorker, ARTICLE_URL);
    await article.evaluate(() => window.scrollTo(0, Math.floor(document.body.scrollHeight * 0.35)));
    await setAskScript(richScript('shot-s1', 'shot-req-2', ANSWER_2));
    await ask(panel, Q2);
    await expect.poll(async () => {
      const payload = await panel.evaluate(async (port) =>
        (await (await fetch(`http://127.0.0.1:${port}/__last-ask`)).json()).lastAsk, MOCK_PORT);
      return String(payload?.question ?? '').includes(Q2);
    }, { timeout: 20_000 }).toBe(true);
    await expect(panel.locator('.msg.assistant')).toHaveCount(2);
    await expect(panel.locator('.msg.assistant.streaming')).toHaveCount(0);
    await expect(panel.locator('.msg.assistant table').first()).toBeVisible();
    // 等真实渲染产物 .mermaid-figure（不能用 svg 泛配——消息工具栏图标也是 svg）
    await expect(panel.locator('.mermaid-figure').first()).toBeVisible({ timeout: 15_000 });
    await scrollPanelBottom(panel);
    await panel.screenshot({ path: path.join(RAW_DIR, 'panel-2.png') });
    await article.screenshot({ path: path.join(RAW_DIR, 'article-2.png') });

    // ---- Shot 3：知识保存（草稿回填 → 保存成功只读视图） ----
    await article.evaluate(() => window.scrollTo(0, Math.floor(document.body.scrollHeight * 0.6)));
    await panel.locator('.keep-entry').first().click();
    await expect(panel.locator('.keep-panel')).toBeVisible();
    await expect(panel.locator('[data-kf="title"]')).not.toHaveValue('', { timeout: 10_000 });
    // 覆盖 mock 草稿为真实感文案（保存响应 item 原样回显，落库 payload 一致）
    await panel.locator('[data-kf="title"]').fill('Cursor 迁移 Codex 的两周适应法');
    await panel.locator('[data-kf="summary"]').fill('补全风格、上下文引用与计费三类差异；建议并行两周逐步替换肌肉记忆');
    await panel.locator('[data-kf="understanding"]').fill('我的理解：迁移节奏比工具选择更重要，先并行再全量切换');
    // 商店图用草稿态（信息量足：标题/摘录/AI 提炼/个人理解/来源）；保存成功视图留作备选
    await panel.screenshot({ path: path.join(RAW_DIR, 'panel-3.png') });
    await panel.locator('[data-kf="save"]').click();
    await expect(panel.locator('.keep-saved')).toBeVisible();
    await expect(panel.locator('[data-kf="svTitle"]')).toHaveText('Cursor 迁移 Codex 的两周适应法');
    // 级联浮现动画完成后才截图（否则卡片空白）
    await expect(panel.locator('[data-kf="svTitle"]')).toHaveCSS('opacity', '1', { timeout: 5_000 });
    await panel.screenshot({ path: path.join(RAW_DIR, 'panel-3-saved.png') });
    await article.screenshot({ path: path.join(RAW_DIR, 'article-3.png') });

    // ---- Promo 瓷砖（440×280 直出） ----
    const promo = await context.newPage();
    await promo.setViewportSize({ width: 440, height: 280 });
    await promo.goto(PROMO_URL);
    await expect(promo.locator('h1')).toBeVisible();
    await promo.screenshot({ path: path.join(RAW_DIR, 'promo-tile.png') });
    await promo.close();
  });
});
