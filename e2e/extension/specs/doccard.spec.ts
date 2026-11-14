// PDF 解读 PR3：文档卡。PDF 标签页识别 → 未读取态 → 读取（mock /api/page/prepare）
// → 已就绪态 → 建议问题透传 documentId；失败态（needs_auth）文案与重试。
// PDF 标签页用 route fulfill 的 text/html 页面模拟（URL 以 .pdf 结尾即可触发
// isPdfUrl；真 PDF 在 headless 下会走下载，tab.url 不可靠）。
import { test, expect, FIXTURE_BASE, MOCK_PORT, closeTestPages, type Page, type BrowserContext, type Worker } from '../fixtures/extension.fixture';

const PDF_URL = `${FIXTURE_BASE}/sample.pdf`;
const PDF_TITLE = 'Research Paper.pdf';
// 问题文本全 spec 唯一（pollLastAsk 按 includes 匹配，防跨文件陈旧数据）
const PROBE_QUESTION = '文档卡未读态提问探针';

type AnyChrome = { chrome: any };

async function resetMock() {
  // null 同时重置 ask/prepare 全部 mock 记录态（见 servers.mjs）
  await fetch(`http://127.0.0.1:${MOCK_PORT}/__ask-script`, {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(null),
  });
}

// 打开 .pdf 后缀的模拟 PDF 标签页（text/html 内容，标题来自 <title>）
async function openPdfTab(context: BrowserContext): Promise<Page> {
  const pdf = await context.newPage();
  await pdf.route('**/sample.pdf', (route) => route.fulfill({
    contentType: 'text/html; charset=utf-8',
    body: `<!DOCTYPE html><html><head><title>${PDF_TITLE}</title></head><body>fake pdf viewer</body></html>`,
  }));
  await pdf.goto(PDF_URL);
  return pdf;
}

// 确认活动标签页是 PDF 页（bringToFront 有异步窗口，按 skill 约定轮询确认）
async function expectPdfActive(serviceWorker: Worker) {
  await expect.poll(async () => serviceWorker.evaluate(async () => {
    const tabs = await (globalThis as AnyChrome).chrome.tabs.query({ active: true, currentWindow: true });
    return tabs[0]?.url || '';
  }), { timeout: 10_000 }).toBe(PDF_URL);
}

async function pollLastPrepare(sidepanel: Page) {
  await expect.poll(async () => sidepanel.evaluate(async (port) => {
    const r = await fetch(`http://127.0.0.1:${port}/__last-prepare`);
    const d = await r.json();
    return (d?.lastPrepare?.url ?? '');
  }, MOCK_PORT), { timeout: 10_000 }).toBe(PDF_URL);
}

async function fetchLastAsk(sidepanel: Page): Promise<{ question: string; documentId: string }> {
  // evaluate 回调整体跑在页面上下文，结果必须经 return 带回（不能回写 Node 侧闭包变量）
  return sidepanel.evaluate(async (port) => {
    const r = await fetch(`http://127.0.0.1:${port}/__last-ask`);
    const d = await r.json();
    return {
      question: String(d?.lastAsk?.question ?? ''),
      documentId: String(d?.lastAsk?.page?.documentId ?? ''),
    };
  }, MOCK_PORT);
}

async function pollLastAsk(sidepanel: Page): Promise<{ question: string; documentId: string }> {
  await expect.poll(async () => (await fetchLastAsk(sidepanel)).question, { timeout: 10_000 }).not.toBe('');
  return fetchLastAsk(sidepanel);
}

test.describe('文档卡（PDF 解读）', () => {
  let sidepanel: Page;

  test.beforeEach(async ({ context, serviceWorker }) => {
    await serviceWorker.evaluate(async () => {
      await (globalThis as AnyChrome).chrome.storage.local.clear();
    });
    await resetMock();
    sidepanel = await context.newPage();
    await sidepanel.goto(`chrome-extension://${new URL(serviceWorker.url()).host}/sidepanel/sidepanel.html`);
    await expect(sidepanel.locator('#questionInput')).toBeVisible();
  });

  test.afterEach(async ({ context }) => {
    await resetMock();
    await closeTestPages(context);
  });

  test('未读取态 → 读取 → 已就绪 → 建议问题透传 documentId', async ({ context, serviceWorker }) => {
    const pdf = await openPdfTab(context);
    await pdf.bringToFront();
    await expectPdfActive(serviceWorker);

    // 卡片出现：未读取态（badge 橙色 + 文件名 + 主按钮）
    await expect(sidepanel.locator('#docCard')).toBeVisible();
    await expect(sidepanel.locator('#docCard .dc-badge-orange')).toHaveText('尚未读取');
    await expect(sidepanel.locator('#docCard .dc-title')).toHaveText(PDF_TITLE);

    // 未读取时提问：payload.page 不带 documentId（维持原降级行为）
    await sidepanel.evaluate((q: string) => {
      const input = document.querySelector('#questionInput')!;
      input.appendChild(document.createTextNode(q));
      input.dispatchEvent(new InputEvent('input', { bubbles: true }));
      (document.querySelector('#askBtn') as HTMLButtonElement).click();
    }, PROBE_QUESTION);
    await expect(sidepanel.locator('.msg.assistant')).toBeVisible();
    await expect(sidepanel.locator('.msg.assistant.streaming')).toHaveCount(0);
    const probe = await pollLastAsk(sidepanel);
    expect(probe.question).toContain(PROBE_QUESTION);
    expect(probe.documentId).toBe('');

    // 点读取：mock 桥默认 ready（28,450 字）→ 已就绪态
    await sidepanel.evaluate(() => {
      const btn = [...document.querySelectorAll('#docCard .dc-cta')]
        .find((b) => b.textContent === '读取文档') as HTMLButtonElement | undefined;
      btn?.click();
    });
    await pollLastPrepare(sidepanel);
    await expect(sidepanel.locator('#docCard .dc-badge-green')).toHaveText('已就绪');
    await expect(sidepanel.locator('#docCard .dc-meta')).toContainText('28,450');
    await expect(sidepanel.locator('#docCard .dc-suggest-item')).toHaveCount(3);

    // 建议问题点击 → 走提问链路并携带 documentId。
    // 先重置 mock 记录态：lastAsk 里还留着上面的探针数据，不重置 poll 会立即命中陈旧值
    await resetMock();
    await sidepanel.evaluate(() => {
      (document.querySelector('#docCard .dc-suggest-item') as HTMLButtonElement).click();
    });
    await expect(sidepanel.locator('.msg.assistant').last()).toBeVisible();
    const asked = await pollLastAsk(sidepanel);
    expect(asked.question).toContain('帮我建立这篇文档的整体理解框架');
    expect(asked.documentId).toBe(PDF_URL);
  });

  test('失败态（needs_auth）渲染原因文案，重试后转已就绪', async ({ context, serviceWorker }) => {
    const pdf = await openPdfTab(context);
    await pdf.bringToFront();
    await expectPdfActive(serviceWorker);
    await expect(sidepanel.locator('#docCard .dc-badge-orange')).toHaveText('尚未读取');

    // 注入 needs_auth 分类状态（200 + status，产品语义非协议错误）
    await fetch(`http://127.0.0.1:${MOCK_PORT}/__prepare-script`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        status: 'needs_auth',
        message: '文档需要登录或访问权限（桌面端不会继承浏览器 Cookie）: mock 403',
      }),
    });
    await sidepanel.evaluate(() => {
      const btn = [...document.querySelectorAll('#docCard .dc-cta')]
        .find((b) => b.textContent === '读取文档') as HTMLButtonElement | undefined;
      btn?.click();
    });
    await pollLastPrepare(sidepanel);
    await expect(sidepanel.locator('#docCard .dc-badge-orange')).toHaveText('需要登录');
    await expect(sidepanel.locator('#docCard .dc-desc')).toContainText('桌面端不会继承浏览器 Cookie');
    await expect(sidepanel.locator('#docCard .dc-cta')).toHaveText('重试');

    // 重试：恢复默认 ready 响应 → 已就绪
    await fetch(`http://127.0.0.1:${MOCK_PORT}/__prepare-script`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(null),
    });
    await sidepanel.evaluate(() => {
      (document.querySelector('#docCard .dc-cta') as HTMLButtonElement).click();
    });
    await expect(sidepanel.locator('#docCard .dc-badge-green')).toHaveText('已就绪');
  });

  test('非 PDF 标签页不显示文档卡', async ({ context, serviceWorker }) => {
    const page = await context.newPage();
    await page.goto(`${FIXTURE_BASE}/article-a.html`);
    await page.bringToFront();
    await expect.poll(async () => serviceWorker.evaluate(async () => {
      const tabs = await (globalThis as AnyChrome).chrome.tabs.query({ active: true, currentWindow: true });
      return tabs[0]?.url || '';
    }), { timeout: 10_000 }).toContain('article-a.html');
    await expect(sidepanel.locator('#docCard')).toBeHidden();
  });
});
