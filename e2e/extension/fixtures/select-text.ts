// 划词选中文本助手：在页面**可见**文本节点里找关键词并构造选区。
// 从 corpus-smoke 抽出共享（spec 文件不能互相 import——模块求值会重复注册用例）。
import type { Page } from '../fixtures/extension.fixture';

export async function selectNeedle(page: Page, needle: string) {
  await page.evaluate((kw) => {
    const skip = (n: Node | null): boolean => {
      for (let el = n?.parentElement; el; el = el.parentElement) {
        const tag = el.tagName;
        if (tag === 'SCRIPT' || tag === 'STYLE' || tag === 'NOSCRIPT' || tag === 'TEMPLATE') return true;
      }
      return false;
    };
    const visible = (n: Text) => !skip(n) && (n.parentElement?.getClientRects().length ?? 0) > 0;
    const pick = (n: Text) => {
      const idx = (n.textContent || '').indexOf(kw);
      if (idx < 0) return false;
      const range = document.createRange();
      range.setStart(n, idx);
      range.setEnd(n, idx + kw.length);
      const sel = window.getSelection()!;
      sel.removeAllRanges();
      sel.addRange(range);
      return true;
    };
    const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
    let node: Text | null;
    while ((node = walker.nextNode() as Text | null)) {
      if (visible(node) && pick(node)) return;
    }
    // 兜底：任何 ≥6 个非空白字符的可见文本节点
    walker.currentNode = document.body;
    while ((node = walker.nextNode() as Text | null)) {
      if (visible(node) && (node.textContent || '').trim().length >= 6) { pick(node); return; }
    }
  }, needle);
}
