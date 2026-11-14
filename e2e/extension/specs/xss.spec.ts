// P1-3：XSS 安全。不可信内容（AI 回复正文、划词文本、提问、回答回显）经渲染链
// 必须只转义不执行。渲染面：AI 正文走 marked + DOMPurify（render.js renderMarkdown）；
// 用户气泡/划词 chip/历史列表走 textContent。断言三层：危险元素不落地、
// 危险属性被剥、onerror 等执行副作用标志位恒为 undefined。
import { test, expect, FIXTURE_BASE, MOCK_PORT, closeTestPages, type Page, type BrowserContext, type Worker } from '../fixtures/extension.fixture';

const MOCK_BASE = `http://127.0.0.1:${MOCK_PORT}`;
const XSS_URL = `${FIXTURE_BASE}/xss.html`;

async function setAskScript(events: unknown[] | null) {
  const resp = await fetch(`${MOCK_BASE}/__ask-script`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(events),
  });
  expect(resp.ok).toBe(true);
}

// 全部执行副作用标志位必须为 undefined（面板页 window 上检查）
async function expectNoExecution(page: Page, keys: string[]) {
  const flags = await page.evaluate((ks) =>
    Object.fromEntries(ks.map((k) => [k, (globalThis as Record<string, unknown>)[k]])), keys);
  for (const k of keys) expect(flags[k], `${k} 不应被执行`).toBeUndefined();
}

test.describe('XSS 安全', () => {
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

  test('AI 输出含 script/img onerror/iframe/javascript 链接 → DOMPurify 只留安全内容，不执行', async () => {
    await setAskScript([
      { type: 'session', sessionId: 'mock-xss-1' },
      {
        type: 'token',
        content: '正常开头。<script>window.__xssScript=1</script>'
          + '<img src=x onerror="window.__xssImg=1">'
          + '<iframe srcdoc="&lt;b&gt;bad&lt;/b&gt;"></iframe>'
          + ' [点我](javascript:window.__xssLink=1) 正常结尾。',
      },
      { done: true, requestId: 'xss-req-1' },
    ]);

    await sidepanel.locator('#questionInput').fill('渲染一下这段');
    await sidepanel.locator('#askBtn').click();

    await expect(sidepanel.locator('.msg.assistant')).toBeVisible();
    await expect(sidepanel.locator('.msg.assistant.streaming')).toHaveCount(0);

    // 危险元素不落地（script/iframe 整体移除；img 保留但危险属性必须被剥）
    await expect(sidepanel.locator('.msg.assistant script')).toHaveCount(0);
    await expect(sidepanel.locator('.msg.assistant iframe')).toHaveCount(0);
    await expect(sidepanel.locator('.msg.assistant img[onerror]')).toHaveCount(0);
    await expect(sidepanel.locator('.msg.assistant a[href^="javascript:"]')).toHaveCount(0);
    // 无执行副作用 + 正常文本保留（DOMPurify 不误伤正文）
    await expectNoExecution(sidepanel, ['__xssScript', '__xssImg', '__xssLink']);
    await expect(sidepanel.locator('.msg.assistant')).toContainText('正常开头。');
    await expect(sidepanel.locator('.msg.assistant')).toContainText('正常结尾。');
  });

  test('划词文本与提问含 HTML 字面量 → chip/用户气泡纯文本渲染，回显经管线不执行', async ({
    context, serviceWorker,
  }) => {
    // 真实页面上的「字面 HTML 文本」（fixture 已实体转义，选区拿到的是纯文本）
    const article = await context.newPage();
    await article.goto(XSS_URL);
    await article.evaluate(() => {
      const node = document.querySelector('#quote')!.firstChild!;
      const full = node.textContent || '';
      const start = full.indexOf('<img');
      const range = document.createRange();
      range.setStart(node, start);
      range.setEnd(node, start + '<img src=x onerror="window.__xssChip=1">'.length);
      const sel = window.getSelection()!;
      sel.removeAllRanges();
      sel.addRange(range);
    });
    const tabId = await serviceWorker.evaluate(async (url: string) => {
      const tabs = await (globalThis as { chrome: any }).chrome.tabs.query({});
      return tabs.find((t: { url?: string }) => t.url === url)?.id;
    }, XSS_URL);
    await sidepanel.evaluate(async (id: number) => {
      await (globalThis as { chrome: any }).chrome.runtime.sendMessage({
        type: 'test-menu-click',
        skipPanelOpen: true, // E2E 不开原生面板
        info: { menuItemId: 'absorb-open-panel', selectionText: '<img src=x onerror>', frameId: 0 },
        tab: { id, url: 'x' },
      });
    }, tabId);
    await expect(sidepanel.locator('.quote-chip')).toHaveCount(1);

    // chip 渲染面：恶意字面文本绝不落成真实 img（可见文本被截断为 5 字，不在此断言文本）
    await expect(sidepanel.locator('.quote-chip img')).toHaveCount(0);
    await expect(sidepanel.locator('.quote-chip')).toBeVisible();

    // 提问文本带 HTML 字面量（有 chip，必须追加文本节点而非 fill）
    const Q = '<img src=x onerror="window.__xssQ=1">';
    await sidepanel.evaluate((q) => {
      const input = document.querySelector('#questionInput')!;
      input.appendChild(document.createTextNode(q));
      input.dispatchEvent(new InputEvent('input', { bubbles: true }));
      (document.querySelector('#askBtn') as HTMLButtonElement).click();
    }, Q);

    // 用户气泡：textContent 路径，字面文本可见、无 img 落地
    await expect(sidepanel.locator('.msg.user').filter({ hasText: 'onerror' })).toBeVisible();
    await expect(sidepanel.locator('.msg.user img')).toHaveCount(0);

    // 助手回显（默认脚本回显问题前 40 字）经 marked + DOMPurify：
    // 恶意 img 保留为元素但 onerror 属性被剥，无执行副作用
    await expect(sidepanel.locator('.msg.assistant')).toBeVisible();
    await expect(sidepanel.locator('.msg.assistant.streaming')).toHaveCount(0);
    await expect(sidepanel.locator('.msg.assistant img[onerror]')).toHaveCount(0);

    await expectNoExecution(sidepanel, ['__xssChip', '__xssQ']);
  });
});
