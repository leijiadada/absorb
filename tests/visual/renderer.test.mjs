// renderer.js 渲染冒烟测试：node:test + 最小 DOM stub（不依赖浏览器，不进扩展 bundle）
// 覆盖：5 个固定案例（TCP 握手/Redis 跳表/RAG 对比/GMP/简单问答不触发）
//       + 工程回归（半截 DSL、畸形组件、XSS、畸形输入不抛异常）
// 说明：mdToHtml 为桩（真链路 marked+DOMPurify 在浏览器端），故只断言内容落地与结构正确。
import test from 'node:test';
import assert from 'node:assert/strict';
import { parseVisualSegments } from '../../extension/sidepanel/js/visual/parser.js';
import { renderVisualBlock } from '../../extension/sidepanel/js/visual/renderer.js';

// ---- 最小 DOM stub ----
function fakeEl(tag) {
  return {
    tagName: tag, children: [], attrs: {}, _text: '', _html: '', _ds: {}, style: {},
    get className() { return this.attrs.class || ''; },
    set className(v) { this.attrs.class = v; },
    get dataset() { return this._ds; },
    set textContent(v) { this._text = String(v); this.children = []; },
    get textContent() { return this._text; },
    set innerHTML(v) { this._html = String(v); this.children = []; },
    get innerHTML() { return this._html; },
    setAttribute(k, v) { this.attrs[k] = v; },
    appendChild(c) { this.children.push(c); return c; },
  };
}
globalThis.document = { createElement: (t) => fakeEl(t) };

const mdToHtml = (md) => `<p>${md.replace(/&/g, '&amp;').replace(/</g, '&lt;')}</p>`;
const walk = (n, fn) => { fn(n); (n.children || []).forEach((c) => walk(c, fn)); };
const find = (n, pred) => { const out = []; walk(n, (x) => { if (pred(x)) out.push(x); }); return out; };
const classes = (n, cls) => find(n, (x) => (x.attrs.class || '').includes(cls));
const exact = (n, cls) => find(n, (x) => x.attrs.class === cls);
const render = (rawLines) => renderVisualBlock(parseVisualSegments(rawLines.join('\n'))[0], mdToHtml);

test('案例1 TCP 三次握手（sheet 完整骨架）', () => {
  const root = render([
    ':::absorb-visual', '---', 'template: sheet', 'title: TCP 三次握手', 'subtitle: 可靠连接如何建立', '---',
    '', '导语：三次握手让双方确认收发能力并同步初始序列号，缺一不可。', '',
    '## 握手过程', '', '```mermaid', 'sequenceDiagram', '    C->>S: SYN', '```', '',
    '## 关键状态', '', '::::timeline', '- SYN_SENT：客户端发出 SYN', '- ESTABLISHED：连接建立', '::::', '',
    '::::callout tip', '**为什么不是两次**：两次无法确认服务端发送能力。', '::::', ':::',
  ]);
  assert.equal(root.dataset.template, 'sheet');
  assert.ok(root.attrs['aria-label'].includes('TCP 三次握手'));
  assert.equal(classes(root, 'av-title').length, 1);
  assert.equal(classes(root, 'av-lead').length, 1);
  assert.equal(exact(root, 'av-panel').length, 2);
  assert.equal(classes(root, 'av-timeline').length, 1);
  assert.equal(classes(root, 'av-callout-tip').length, 1);
  assert.ok(find(root, (n) => (n._html || '').includes('mermaid')).length >= 1, 'mermaid 块进入 DOM（待 decorateMermaid）');
});

test('案例2 Redis 跳表（tree + kv + 表格对比）', () => {
  const root = render([
    ':::absorb-visual', '---', 'title: Redis 跳表', '---', '导语。', '',
    '## 结构', '', '::::tree', '- 跳表', '  - Level 2 稀疏索引', '  - Level 1 有序链表', '- 哈希表', '::::', '',
    '## 属性', '', '::::kv', '平均复杂度 :: O(log n)', '实现 :: 多级稀疏索引', '::::', '',
    '## 对比', '', '| 维度 | 跳表 | 红黑树 |', '| --- | --- | --- |', '| 实现复杂度 | 低 | 高 |', ':::',
  ]);
  assert.ok(classes(root, 'av-tree').length >= 1);
  assert.equal(exact(root, 'av-kv').length, 1);
  assert.ok(find(root, (n) => (n._html || '').includes('| 维度 | 跳表')).length >= 1, '表格源码进入 DOM（真链路由 marked 转 <table>）');
});

test('案例3 RAG 对比（doc 模板 + warn）', () => {
  const root = render([
    ':::absorb-visual', '---', 'template: doc', 'title: RAG vs 微调', '---', '导语。', '',
    '## 选型对比', '', '| 维度 | RAG | 微调 |', '| --- | --- | --- |', '| 时效性 | 好 | 差 |', '',
    '::::callout warn', '两者不互斥，可组合。', '::::', ':::',
  ]);
  assert.equal(root.dataset.template, 'doc');
  assert.equal(classes(root, 'av-callout-warn').length, 1);
});

test('案例4 Go 内存管理（doc + flowchart + kv）', () => {
  const root = render([
    ':::absorb-visual', '---', 'template: doc', 'title: Go 内存管理', '---', '导语。', '',
    '## 分配路径', '', '```mermaid', 'flowchart TD', '    A["mcache"] --> B["mcentral"]', '```', '',
    '## 参数', '', '::::kv', '触发 GC :: heaplive 达标', '::::', ':::',
  ]);
  assert.ok(find(root, (n) => (n._html || '').includes('mcache')).length >= 1);
  assert.equal(exact(root, 'av-kv').length, 1);
});

test('案例5 简单问答不产生可视化段', () => {
  const segs = parseVisualSegments('Redis 是内存数据库，2-3 句讲完。');
  assert.equal(segs.length, 1);
  assert.equal(segs[0].type, 'md');
});

test('半截 DSL（未闭合）宽容渲染已收到的内容', () => {
  const root = render([':::absorb-visual', '---', 'title: 半截', '---', '已有内容', '## 节', '进行中……']);
  assert.ok(JSON.stringify(root).includes('进行中'));
});

test('畸形组件（未知名）整块退化为 markdown', () => {
  const root = render([':::absorb-visual', '::::gantt', 'x', '::::', ':::']);
  assert.ok(JSON.stringify(root).includes('::::gantt'));
});

test('chart bar：结构、条宽按最大值归一、正值下限 2%', () => {
  const root = render([
    ':::absorb-visual', '---', 'title: T', '---',
    '## 耗时', '', '::::chart bar', 'title: 构建耗时', 'unit: 秒', 'Go :: 100', 'Rust :: 2', 'Java :: 0', '::::', ':::',
  ]);
  const chart = exact(root, 'av-chart');
  assert.equal(chart.length, 1);
  assert.ok(chart[0].attrs['aria-label'].includes('条形图：构建耗时'));
  const title = classes(root, 'av-chart-title');
  assert.equal(title.length, 1);
  assert.equal(title[0]._text, '构建耗时（单位：秒）');
  const bars = classes(root, 'av-chart-bar');
  assert.equal(bars.length, 3);
  assert.equal(bars[0].style.width, '100%');          // 最大值满宽
  assert.equal(bars[1].style.width, '2%');            // 2/100 → 下限 2%
  assert.equal(bars[2].style.width, '0%');            // 0 不触发下限
  const vals = classes(root, 'av-chart-value').map((n) => n._text);
  assert.deepEqual(vals, ['100', '2', '0']);
  const labels = classes(root, 'av-chart-label').map((n) => n._text);
  assert.deepEqual(labels, ['Go', 'Rust', 'Java']);
});

test('chart 降级：零有效数据退化为原文，不渲染空图', () => {
  const root = render([':::absorb-visual', '::::chart bar', '只有一句话', '::::', ':::']);
  assert.equal(exact(root, 'av-chart').length, 0);
  assert.ok(JSON.stringify(root).includes('只有一句话'));
});

test('XSS：frontmatter 标题走 textContent，正文经 mdToHtml 转义', () => {
  const root = render([':::absorb-visual', '---', 'title: <img src=x onerror=alert(1)>', '---', '<script>alert(2)</script>', ':::']);
  assert.equal(find(root, (n) => n._text.includes('<img')).length, 1, '标题不解析为 HTML');
  assert.ok(!JSON.stringify(root).includes('<script>'), '正文被转义（真链路为 DOMPurify）');
});

test('畸形输入（控制字符/空标题）不抛异常', () => {
  assert.doesNotThrow(() => renderVisualBlock(parseVisualSegments(':::absorb-visual\n\n## \x01\x02\x03 weird\n:::')[0], mdToHtml));
});
