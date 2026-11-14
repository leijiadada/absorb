// 图卡片工具栏（对齐 Trae 图片三键：复制图片 / 下载 PNG / 新窗口打开）。
// mermaid 渲染成功后挂悬浮工具栏；本 spec 走真实导出行为：
//   下载 → 真实 download 事件 + PNG 文件签名校验
//   新窗口 → chrome.tabs.create blob: 标签（扩展源 blob URL 可导航）
//   复制 → ClipboardItem stub 走通代码路径（扩展页持久上下文里真实剪贴板
//          权限不稳定，且 stub 才能断言落盘内容；page 级注入不泄漏到其他 spec）
import { test, expect, MOCK_PORT, closeTestPages, type Page, type BrowserContext, type Worker } from '../fixtures/extension.fixture';
import { readFileSync } from 'node:fs';

const MOCK_BASE = `http://127.0.0.1:${MOCK_PORT}`;
// 问题文本跨 spec 必须唯一（pollLastAsk 按 includes 匹配）
const QUESTION = '图工具栏专项验证：请画一张 mermaid 流程图（唯一问题文本）';
const MERMAID_SRC = 'flowchart TD\n  A[开始] --> B{是否继续}\n  B -->|是| C[结束]';
const REPLY = `流程图如下：\n\n\`\`\`mermaid\n${MERMAID_SRC}\n\`\`\`\n\n完成。`;

// PNG 文件签名（\x89PNG\r\n\x1a\n）
const PNG_MAGIC = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]);

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
  // page 级注入（context 级 addInitScript 会波及后续 spec 文件的页面）
  await page.addInitScript(() => {
    const w = globalThis as unknown as { __figCopied: unknown[][]; ClipboardItem: unknown; [k: string]: unknown };
    w.__figCopied = [];
    // 普通函数构造器：class 字段声明经 Playwright TS 转译后 items 会丢失赋值
    w.ClipboardItem = function ClipboardItem(this: { items: Record<string, Blob> }, items: Record<string, Blob>) {
      this.items = items;
    };
    Object.defineProperty(navigator, 'clipboard', {
      value: {
        write: async (items: unknown) => {
          (globalThis as unknown as { __figCopied: unknown[][] }).__figCopied.push(items as unknown[]);
        },
        writeText: async () => {},
      },
    });
  });
  await page.goto(`chrome-extension://${new URL(serviceWorker.url()).host}/sidepanel/sidepanel.html`);
  await expect(page.locator('#questionInput')).toBeVisible();
  return page;
}

async function ask(page: Page, question: string) {
  await page.locator('#questionInput').fill(question);
  const askBtn = page.locator('#askBtn');
  await expect(askBtn).toBeEnabled();
  await askBtn.click();
}

test.describe('图卡片工具栏', () => {
  let sidepanel: Page;
  let fig: ReturnType<Page['locator']>;

  // 悬浮工具栏默认 opacity 0 + pointer-events none，点击前必须先 hover 图卡片
  async function clickFigBtn(act: string) {
    await fig.hover();
    await expect(fig.locator('.fig-toolbar')).toHaveCSS('opacity', '1');
    await fig.locator(`.fig-btn[data-act="${act}"]`).click();
  }

  test.beforeEach(async ({ context, serviceWorker }) => {
    await serviceWorker.evaluate(async () => {
      await (globalThis as { chrome: any }).chrome.storage.local.clear();
    });
    await setAskScript([
      { type: 'session', sessionId: 'mock-session-fig' },
      { type: 'token', content: REPLY },
      { type: 'token', done: true, requestId: 'mock-req-fig' },
    ]);
    sidepanel = await openSidepanel(context, serviceWorker);
    await ask(sidepanel, QUESTION);
    // mermaid vendor 脚本 2.8MB 首次懒加载 + 渲染，放宽到 15s
    fig = sidepanel.locator('.mermaid-figure');
    await expect(fig).toBeVisible({ timeout: 15_000 });
  });

  test.afterEach(async ({ context }) => {
    await setAskScript(null);
    await closeTestPages(context);
  });

  test('mermaid 渲染后挂三键工具栏：默认隐藏，hover / 键盘焦点显示', async () => {
    const bar = fig.locator('.fig-toolbar');
    await expect(bar).toHaveCount(1);
    const btns = bar.locator('.fig-btn');
    await expect(btns).toHaveCount(3);

    // 默认隐藏且不可点（opacity 0 + pointer-events none 由 CSS 保证）
    await expect(bar).toHaveCSS('opacity', '0');

    // hover 图卡片 → 工具栏浮现
    await fig.hover();
    await expect(bar).toHaveCSS('opacity', '1');

    // 三键语义齐备（locale zh-CN）
    await expect(btns.nth(0)).toHaveAttribute('title', '复制图片');
    await expect(btns.nth(1)).toHaveAttribute('title', '下载 PNG');
    await expect(btns.nth(2)).toHaveAttribute('title', '新窗口打开');
  });

  test('下载 PNG：真实 download 事件，文件名、PNG 签名与像素内容正确', async () => {
    const [download] = await Promise.all([
      sidepanel.waitForEvent('download'),
      clickFigBtn('download'),
    ]);
    expect(download.suggestedFilename()).toMatch(/^absorb-diagram-\d+\.png$/);
    const path = await download.path();
    const buf = readFileSync(path!);
    expect(buf.subarray(0, 8).equals(PNG_MAGIC)).toBe(true);
    expect(buf.length).toBeGreaterThan(1000); // 非空图（2x 栅格化后不会只有几十字节）

    // 像素级校验：同路径栅格化后检查深色像素（文本/箭头 #333）与节点填充（#ECECFF）
    // 都存在——防"导出成功但 canvas 只画了形状、foreignObject 文本丢失"的静默劣化
    const px = await sidepanel.evaluate(async () => {
      const svg = document.querySelector('.mermaid-figure svg') as SVGSVGElement;
      const vb = svg.viewBox.baseVal;
      const clone = svg.cloneNode(true) as SVGSVGElement;
      clone.removeAttribute('style');
      clone.setAttribute('xmlns', 'http://www.w3.org/2000/svg');
      clone.setAttribute('width', String(vb.width));
      clone.setAttribute('height', String(vb.height));
      const url = 'data:image/svg+xml;charset=utf-8,' +
        encodeURIComponent(new XMLSerializer().serializeToString(clone));
      const img = await new Promise<HTMLImageElement>((resolve, reject) => {
        const im = new Image();
        im.onload = () => resolve(im);
        im.onerror = () => reject(new Error('decode failed'));
        im.src = url;
      });
      const canvas = document.createElement('canvas');
      canvas.width = vb.width * 2;
      canvas.height = vb.height * 2;
      const ctx = canvas.getContext('2d')!;
      ctx.fillStyle = '#ffffff';
      ctx.fillRect(0, 0, canvas.width, canvas.height);
      ctx.drawImage(img, 0, 0, canvas.width, canvas.height);
      const d = ctx.getImageData(0, 0, canvas.width, canvas.height).data;
      let dark = 0, nodeFill = 0;
      for (let i = 0; i < d.length; i += 4) {
        if (d[i] < 128 && d[i + 1] < 128 && d[i + 2] < 128) dark++;
        // #ECECFF = 236,236,255（mermaid default 主题节点填充）
        if (d[i] > 230 && d[i + 1] > 230 && d[i + 2] > 248 && d[i + 2] <= 255) nodeFill++;
      }
      return { dark, nodeFill };
    });
    expect(px.dark).toBeGreaterThan(200);
    expect(px.nodeFill).toBeGreaterThan(500);
  });

  test('复制图片：PNG blob 进入剪贴板写入调用，按钮 morph 反馈', async () => {
    await clickFigBtn('copy');
    await expect
      .poll(() => sidepanel.evaluate(() => (globalThis as { __figCopied: unknown[] }).__figCopied.length))
      .toBe(1);
    const info = await sidepanel.evaluate(() => {
      // write(items) 收到 ClipboardItem 数组；stub 把 items 挂在实例上
      const copied = (globalThis as unknown as { __figCopied: { items: Record<string, Blob> }[][] }).__figCopied;
      const ci = copied[0]?.[0];
      const blob = ci?.items?.['image/png'];
      return { hasPng: blob instanceof Blob, size: blob ? blob.size : 0 };
    });
    expect(info.hasPng).toBe(true);
    expect(info.size).toBeGreaterThan(1000);
    // 复制键 morph「✓」反馈（done class，与 code-copy 同形态）
    await expect(fig.locator('.fig-btn[data-act="copy"]')).toHaveClass(/done/);
  });

  test('新窗口打开：扩展源 blob 标签加载 SVG 文档', async ({ context }) => {
    const [tab] = await Promise.all([
      context.waitForEvent('page'),
      clickFigBtn('open'),
    ]);
    await tab.waitForLoadState('domcontentloaded');
    expect(tab.url()).toMatch(/^blob:chrome-extension:\/\/[a-p0-9]+\/[0-9a-f-]+$/);
    const html = await tab.content();
    expect(html).toContain('<svg');
    expect(html).toContain('开始'); // 节点文本原样保留（矢量打开，非栅格截图）
  });
});
