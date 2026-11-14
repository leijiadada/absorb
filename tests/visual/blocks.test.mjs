// blocks.js 编排与失败态测试：node:test + 最小 DOM stub（不依赖浏览器，不进扩展 bundle）
// 覆盖胶水层此前完全未测的两条承诺（SPEC 设计原则 3 失败降级 / 4 流式不渲染）：
// 流式占位、渲染异常降级、final 分支、解析错误只在终帧收集。
import test from 'node:test';
import assert from 'node:assert/strict';
import { buildVisualContent, makeDegraded, makePending } from '../../extension/sidepanel/js/visual/blocks.js';

// ---- 最小 DOM stub（与 renderer.test.mjs 同款） ----
function fakeEl(tag, cls) {
  const node = {
    tagName: tag, children: [], attrs: {}, _text: '', _html: '',
    get className() { return this.attrs.class || ''; },
    set className(v) { this.attrs.class = v; },
    set textContent(v) { this._text = String(v); this.children = []; },
    get textContent() { return this._text; },
    set innerHTML(v) { this._html = String(v); this.children = []; },
    get innerHTML() { return this._html; },
    setAttribute(k, v) { this.attrs[k] = v; },
    appendChild(c) { this.children.push(c); return c; },
  };
  if (cls) node.className = cls;
  return node;
}

const mdToHtml = (md) => `<p>${md.replace(/&/g, '&amp;').replace(/</g, '&lt;')}</p>`;
const dict = {
  'visual.degraded': '可视化页面渲染失败，以下为原始内容',
  'visual.generating': '正在生成可视化页面…',
};
const deps = () => ({ el: fakeEl, mdToHtml, text: (k) => dict[k] || k });

const VISUAL = [':::absorb-visual', '---', 'title: T', '---', '正文', ':::'].join('\n');

test('纯 Markdown → 只有 md 节点，按原文渲染', () => {
  const nodes = buildVisualContent('一段普通回答', true, deps(), () => {
    throw new Error('不应调用');
  }, []);
  assert.equal(nodes.length, 1);
  assert.equal(nodes[0].innerHTML, mdToHtml('一段普通回答'));
});

test('流式帧（final=false）：visual 段只出占位，不调用渲染器', () => {
  let called = 0;
  const nodes = buildVisualContent(VISUAL, false, deps(), () => { called++; return fakeEl('section'); }, []);
  assert.equal(called, 0);
  assert.equal(nodes.length, 1);
  assert.equal(nodes[0].attrs.class, 'absorb-visual-pending');
  assert.equal(nodes[0].attrs['aria-live'], 'polite');
  assert.equal(nodes[0].textContent, dict['visual.generating']);
});

test('终帧（final=true）：visual 段交给注入的渲染器', () => {
  let got = null;
  const nodes = buildVisualContent(VISUAL, true, deps(), (seg) => {
    got = seg;
    return fakeEl('section');
  }, []);
  assert.equal(nodes.length, 1);
  assert.ok(got.raw.startsWith(':::absorb-visual'));
  assert.equal(nodes[0].tagName, 'section');
});

test('渲染器抛异常 → 降级为原始 Markdown，并记录原因', () => {
  const errors = [];
  const nodes = buildVisualContent(VISUAL, true, deps(), () => {
    throw new Error('boom');
  }, errors);
  assert.equal(nodes.length, 1);
  assert.equal(nodes[0].attrs.class, 'absorb-visual-degraded');
  const [tip, body] = nodes[0].children;
  assert.equal(tip.attrs.class, 'absorb-visual-degraded-tip');
  assert.ok(tip.textContent.includes('渲染异常：boom'));
  // raw 按 parseVisualSegments 约定不含闭合行，这里只断言原始内容被完整渲染
  assert.ok(body.innerHTML.includes(':::absorb-visual'));
  assert.ok(body.innerHTML.includes('正文'));
  assert.equal(errors.length, 1);
  assert.equal(errors[0].component, 'absorb-visual');
});

test('降级框：无 errors 时只显示基础文案（不出空括号）', () => {
  const box = makeDegraded('原始内容', [], deps());
  assert.equal(box.children[0].textContent, dict['visual.degraded']);
});

test('占位框直接构建：类名与 aria-live 正确', () => {
  const box = makePending(deps());
  assert.equal(box.attrs.class, 'absorb-visual-pending');
  assert.equal(box.attrs['aria-live'], 'polite');
  assert.equal(box.textContent, dict['visual.generating']);
});

test('混合内容保持原文顺序（md + visual + md）', () => {
  const mixed = ['前记', ':::absorb-visual', '正文', ':::', '后记'].join('\n');
  const nodes = buildVisualContent(mixed, true, deps(), () => fakeEl('section'), []);
  assert.equal(nodes.length, 3);
  assert.equal(nodes[0].tagName, 'div');
  assert.equal(nodes[1].tagName, 'section');
  assert.equal(nodes[2].tagName, 'div');
  assert.ok(nodes[2].innerHTML.includes('后记'));
});

test('解析错误只在终帧收集，流式帧不收集', () => {
  const unclosed = [':::absorb-visual', '半截内容'].join('\n');
  const e1 = [];
  buildVisualContent(unclosed, true, deps(), () => fakeEl('section'), e1);
  assert.ok(e1.some((e) => e.component === 'absorb-visual' && /未闭合/.test(e.message)));
  const e2 = [];
  buildVisualContent(unclosed, false, deps(), () => fakeEl('section'), e2);
  assert.equal(e2.length, 0);
});
