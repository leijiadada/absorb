// P0-5：知识保存（Knowledge Save）。划词问答 → 吸收入口 → 面板草稿 → 保存 →
// 幂等重复 → 失败提示。协议以 net.js 的 fetchKnowledgeDraft/saveKnowledge 为准。
import { test, expect, FIXTURE_BASE, MOCK_PORT, closeTestPages, type Page, type BrowserContext, type Worker } from '../fixtures/extension.fixture';

const URL_A = `${FIXTURE_BASE}/article-a.html`;
const FRAGMENT = '为大语言模型提供外部上下文';
const UNDERSTANDING = '我的理解：RAG 靠检索外部资料增强回答';

type AnyChrome = { chrome: any };

async function setAskScript(sessionId: string, requestId: string) {
  await fetch(`http://127.0.0.1:${MOCK_PORT}/__ask-script`, {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify([
      { type: 'session', sessionId },
      { type: 'token', content: 'E2E 回答：检索增强生成的解释。' },
      { done: true, requestId },
    ]),
  });
}

// 划词采集 → 带引用提问 → 等回答完成。返回面板页（assistant 气泡已就绪）
async function captureAskAndFinish(context: BrowserContext, serviceWorker: Worker, sidepanel: Page) {
  const article = await context.newPage();
  await article.goto(URL_A);
  await article.evaluate(() => {
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
  const tabId = await serviceWorker.evaluate(async (url: string) => {
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
  }, tabId);
  await expect(sidepanel.locator('.quote-chip')).toHaveCount(1);

  // 输入不能 fill（会清掉 chip），追加文本节点保留引用
  await sidepanel.evaluate(() => {
    const input = document.querySelector('#questionInput')!;
    input.appendChild(document.createTextNode('结合引用解释'));
    input.dispatchEvent(new InputEvent('input', { bubbles: true }));
  });
  await sidepanel.evaluate(() => (document.querySelector('#askBtn') as HTMLButtonElement).click());
  await expect(sidepanel.locator('.msg.assistant')).toBeVisible();
  await expect(sidepanel.locator('.msg.assistant.streaming')).toHaveCount(0);
}

// 打开吸收入口并等草稿填充完成
async function openKeepPanel(page: Page) {
  await page.locator('.keep-entry').click();
  await expect(page.locator('.keep-panel')).toBeVisible();
  // /api/knowledge/draft 自动填充
  await expect(page.locator('[data-kf="title"]')).toHaveValue('E2E 草稿标题', { timeout: 10_000 });
  await expect(page.locator('[data-kf="summary"]')).toHaveValue('E2E 草稿摘要');
  // 原文摘录块引用划词文本
  await expect(page.locator('.keep-quote')).toContainText(FRAGMENT);
}

test.describe('知识保存', () => {
  let sidepanel: Page;

  test.beforeEach(async ({ context, serviceWorker }) => {
    await serviceWorker.evaluate(async () => {
      await (globalThis as { chrome: any }).chrome.storage.local.clear();
    });
    await fetch(`http://127.0.0.1:${MOCK_PORT}/__knowledge-fail`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ on: false }),
    });
    sidepanel = await context.newPage();
    await sidepanel.goto(`chrome-extension://${new URL(serviceWorker.url()).host}/sidepanel/sidepanel.html`);
    await expect(sidepanel.locator('#questionInput')).toBeVisible();
  });

  test.afterEach(async ({ context }) => {
    await fetch(`http://127.0.0.1:${MOCK_PORT}/__knowledge-fail`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ on: false }),
    });
    await fetch(`http://127.0.0.1:${MOCK_PORT}/__ask-script`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(null), // 还原默认脚本，避免泄漏到后续 spec
    });
    await closeTestPages(context);
  });

  test('吸收保存全链路：草稿回填 → 个人理解 → 落库 → 只读视图 + 入口定格', async ({ context, serviceWorker }) => {
    await setAskScript('mock-kb-s1', 'kb-req-1');
    await captureAskAndFinish(context, serviceWorker, sidepanel);

    await openKeepPanel(sidepanel);
    await sidepanel.locator('[data-kf="understanding"]').fill(UNDERSTANDING);
    await sidepanel.locator('[data-kf="save"]').click();

    // 只读视图：标题/摘要以响应 item 为准，无重复标记
    await expect(sidepanel.locator('.keep-saved')).toBeVisible();
    await expect(sidepanel.locator('[data-kf="svTitle"]')).toHaveText('E2E 草稿标题');
    await expect(sidepanel.locator('[data-kf="svSummary"]')).toHaveText('E2E 草稿摘要');
    await expect(sidepanel.locator('[data-kf="svDup"]')).toBeHidden();
    // 入口定格「已保存」并禁用，不再诱导重复
    await expect(sidepanel.locator('.keep-entry.is-done')).toBeDisabled();

    // 落库 payload：标题/摘要/个人理解/来源
    const { lastSave } = await sidepanel.evaluate(async (port) =>
      (await (await fetch(`http://127.0.0.1:${port}/__last-save`)).json()), MOCK_PORT);
    expect(lastSave.title).toBe('E2E 草稿标题');
    expect(lastSave.aiSummary).toBe('E2E 草稿摘要');
    expect(lastSave.userUnderstanding).toBe(UNDERSTANDING);
    expect(lastSave.originSessionId).toBe('mock-kb-s1');
    expect(lastSave.originEventId).toBe('kb-req-1');
    // 来源证据：引用来源带 A 页真实 URL 与原文，页面来源带标题
    const sourcesJson = JSON.stringify(lastSave.sources);
    expect(sourcesJson).toContain(URL_A);
    expect(sourcesJson).toContain(FRAGMENT);
  });

  test('幂等重复：重载面板后同回答再存 → created:false → 重复说明行', async ({ context, serviceWorker }) => {
    await setAskScript('mock-kb-s2', 'kb-req-2');
    await captureAskAndFinish(context, serviceWorker, sidepanel);
    await openKeepPanel(sidepanel);
    await sidepanel.locator('[data-kf="save"]').click();
    await expect(sidepanel.locator('.keep-saved')).toBeVisible();
    await expect(sidepanel.locator('.keep-entry.is-done')).toBeDisabled(); // 入口定格，UI 不诱导重复

    // 真实重复路径：重载面板（历史恢复渲染入口，不携带已存态）→ 再开同回答的入口再存
    await sidepanel.reload();
    await expect(sidepanel.locator('#questionInput')).toBeVisible();
    await sidepanel.locator('.keep-entry').click();
    await expect(sidepanel.locator('.keep-panel')).toBeVisible();
    await sidepanel.locator('[data-kf="save"]').click();

    await expect(sidepanel.locator('.keep-saved')).toBeVisible();
    await expect(sidepanel.locator('[data-kf="svDup"]')).toBeVisible();
    await expect(sidepanel.locator('[data-kf="svCheck"]')).toHaveText('已保存过');
  });

  test('保存失败：错误提示可见、已填内容保留、可重试', async ({ context, serviceWorker }) => {
    await setAskScript('mock-kb-s3', 'kb-req-3');
    await captureAskAndFinish(context, serviceWorker, sidepanel);
    await openKeepPanel(sidepanel);
    await sidepanel.locator('[data-kf="understanding"]').fill(UNDERSTANDING);

    await fetch(`http://127.0.0.1:${MOCK_PORT}/__knowledge-fail`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ on: true }),
    });
    await sidepanel.locator('[data-kf="save"]').click();

    // 错误展示、内容保留、按钮恢复可重试（.keep-saved 常驻 DOM，断言 hidden）
    // 注：net.js 对 HTTP 错误统一映射为 errors.knowledge.saveFailed 本地化文案（不透传服务端 msg）
    await expect(sidepanel.locator('.keep-saved')).toBeHidden();
    await expect(sidepanel.locator('[data-kf="error"]')).toBeVisible();
    await expect(sidepanel.locator('[data-kf="error"]')).toContainText('保存失败');
    await expect(sidepanel.locator('[data-kf="title"]')).toHaveValue('E2E 草稿标题');
    await expect(sidepanel.locator('[data-kf="understanding"]')).toHaveValue(UNDERSTANDING);
    await expect(sidepanel.locator('[data-kf="save"]')).toBeEnabled();

    // 恢复后重试成功
    await fetch(`http://127.0.0.1:${MOCK_PORT}/__knowledge-fail`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ on: false }),
    });
    await sidepanel.locator('[data-kf="save"]').click();
    await expect(sidepanel.locator('.keep-saved')).toBeVisible();
  });
});
