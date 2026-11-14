// parser.js 单元测试：node --test 运行（无外部依赖，文件位于 tests/ 不进扩展 bundle）
// 覆盖：分段（围栏感知/闭合规则/未闭合）、frontmatter、分节、组件解析、降级、CRLF
import test from 'node:test';
import assert from 'node:assert/strict';
import { parseVisualSegments, parseVisualDocument, fixMarkdownTables } from '../../extension/sidepanel/js/visual/parser.js';

// ---------- parseVisualSegments ----------

test('纯 Markdown 无容器 → 单 md 段', () => {
  const segs = parseVisualSegments('# 标题\n\n正文');
  assert.equal(segs.length, 1);
  assert.equal(segs[0].type, 'md');
  assert.equal(segs[0].text, '# 标题\n\n正文');
});

test('基本容器切分：md + visual + md', () => {
  const input = [
    '前言',
    ':::absorb-visual',
    '---',
    'template: sheet',
    'title: T',
    '---',
    '正文',
    ':::',
    '后记',
  ].join('\n');
  const segs = parseVisualSegments(input);
  assert.deepEqual(segs.map((s) => s.type), ['md', 'visual', 'md']);
  assert.match(segs[0].text, /^前言\n?$/);
  assert.match(segs[2].text, /^\n?后记$/);
  assert.ok(segs[1].raw.startsWith(':::absorb-visual'));
  assert.ok(segs[1].closed);
});

test('闭合规则：裸冒号数与开行相等才闭合', () => {
  const segs = parseVisualSegments(':::absorb-visual\nA\n:::\n尾');
  assert.equal(segs[0].type, 'visual');
  assert.ok(segs[0].closed);
  assert.equal(segs[1].text, '尾');
  // 4 冒号裸行不关闭 3 冒号容器（那是组件的闭合行）
  const more = parseVisualSegments(':::absorb-visual\nA\n::::\nB\n:::\n尾');
  assert.ok(more[0].raw.includes('B'));
  assert.equal(more.length, 2);
});

test('围栏感知：容器内代码围栏中的冒号行不闭合', () => {
  const input = [':::absorb-visual', '```text', ':::', '```', '内容', ':::', '尾'].join('\n');
  const segs = parseVisualSegments(input);
  assert.equal(segs.length, 2);
  assert.ok(segs[0].closed);
  assert.ok(segs[0].raw.includes('内容'));
  assert.equal(segs[1].text, '尾');
});

test('4 冒号组件闭行不关闭 3 冒号外层容器', () => {
  const input = [':::absorb-visual', '::::kv', 'a :: b', '::::', 'after', ':::', '尾'].join('\n');
  const segs = parseVisualSegments(input);
  assert.equal(segs.length, 2);
  assert.ok(segs[0].raw.includes('after'));
});

test('未闭合到文末 → closed:false', () => {
  const segs = parseVisualSegments('x\n:::absorb-visual\n半截内容');
  assert.equal(segs.length, 2);
  assert.equal(segs[1].type, 'visual');
  assert.equal(segs[1].closed, false);
});

test('围栏内的容器开行不触发分段', () => {
  const input = '```\n:::absorb-visual\n```\n尾';
  const segs = parseVisualSegments(input);
  assert.equal(segs.length, 1);
  assert.equal(segs[0].type, 'md');
});

test('CRLF 行尾兼容', () => {
  const input = ':::absorb-visual\r\nA\r\n:::\r\n尾';
  const segs = parseVisualSegments(input);
  assert.deepEqual(segs.map((s) => s.type), ['visual', 'md']);
  assert.ok(segs[0].closed);
});

test('多个容器与空内容边界', () => {
  const two = parseVisualSegments(':::absorb-visual\na\n:::\n中\n:::absorb-visual\nb\n:::');
  assert.equal(two.filter((s) => s.type === 'visual').length, 2);
  assert.deepEqual(parseVisualSegments(''), []);
  assert.deepEqual(parseVisualSegments(null), []);
});

// ---------- parseVisualDocument ----------

test('frontmatter 解析：key/value、引号剥离、未知键忽略', () => {
  const raw = [':::absorb-visual', '---', 'template: doc', 'title: "TCP 三次握手"', 'subtitle: 副标题', 'theme: dark', '---', '正文', ':::'].join('\n');
  const doc = parseVisualDocument(raw);
  assert.equal(doc.meta.template, 'doc');
  assert.equal(doc.meta.title, 'TCP 三次握手');
  assert.equal(doc.meta.subtitle, '副标题');
  assert.ok(!('theme' in doc.meta));
});

test('frontmatter 未闭合 → 自愈：键行进 meta，正文不吞', () => {
  const raw = [':::absorb-visual', '---', 'title: T', '正文', ':::'].join('\n');
  const doc = parseVisualDocument(raw);
  assert.equal(doc.meta.title, 'T'); // 键行自愈进元信息（2026-11 泄漏修复：不再漏成可见正文）
  const allMd = [...doc.preamble.blocks, ...doc.sections.flatMap((s) => s.blocks)].map((b) => b.text || '').join('\n');
  assert.ok(allMd.includes('正文')); // 正文仍不吞
  assert.ok(!allMd.includes('title: T'));
});

test('frontmatter 整体缺 --- → 自愈路径 A：键行串进 meta', () => {
  const raw = [':::absorb-visual', 'template: sheet', 'title: 图入门', 'subtitle: 副标题', '', '导语段落', '## 面板', '正文', ':::'].join('\n');
  const doc = parseVisualDocument(raw);
  assert.equal(doc.meta.template, 'sheet');
  assert.equal(doc.meta.title, '图入门');
  assert.equal(doc.meta.subtitle, '副标题');
  const allMd = [...doc.preamble.blocks, ...doc.sections.flatMap((s) => s.blocks)].map((b) => b.text || '').join('\n');
  assert.ok(allMd.includes('导语段落'));
  assert.ok(!allMd.includes('template:'));
  assert.ok(!allMd.includes('title:'));
});

test('frontmatter 自愈不误伤：无键行时正文原样保留', () => {
  const raw = [':::absorb-visual', '普通开头段落', '', '## 面板', 'kv 用法见 title: 字段说明', ':::'].join('\n');
  const doc = parseVisualDocument(raw);
  const allMd = [...doc.preamble.blocks, ...doc.sections.flatMap((s) => s.blocks)].map((b) => b.text || '').join('\n');
  assert.ok(allMd.includes('普通开头段落'));
  assert.ok(allMd.includes('title: 字段说明')); // 非首行键样式不做自愈，正文原样
});

test('分节：## 开节、### 留在节内、前导区独立', () => {
  const raw = [
    ':::absorb-visual',
    '导语段落',
    '',
    '## A. 过程',
    'A 正文',
    '### A.1 细节',
    'A1 正文',
    '## B. 对比',
    'B 正文',
    ':::',
  ].join('\n');
  const doc = parseVisualDocument(raw);
  const mdText = (b) => (b.kind === 'md' ? b.text : `<${b.kind}>`);
  assert.equal(doc.preamble.blocks.map(mdText).join('|').trim(), '导语段落');
  assert.deepEqual(doc.sections.map((s) => s.title), ['A. 过程', 'B. 对比']);
  const a = doc.sections[0].blocks.map(mdText).join('|');
  assert.ok(a.includes('A 正文') && a.includes('### A.1 细节') && a.includes('A1 正文'));
});

test('callout：variant 解析与非白名单回退 info', () => {
  const raw = [':::absorb-visual', '::::callout warn', '小心', '::::', '::::callout', '默认', '::::', ':::'].join('\n');
  const doc = parseVisualDocument(raw);
  const [w, d] = doc.preamble.blocks;
  assert.deepEqual(w, { kind: 'callout', variant: 'warn', text: '小心' });
  assert.deepEqual(d, { kind: 'callout', variant: 'info', text: '默认' });
});

test('kv：分隔、续行并入、无分隔首行丢弃', () => {
  const raw = [':::absorb-visual', '::::kv', 'k1 :: v1', 'k2 :: v2', '续行内容', '残句无分隔', '::::', ':::'].join('\n');
  const doc = parseVisualDocument(raw);
  assert.deepEqual(doc.preamble.blocks[0].items, [
    { key: 'k1', value: 'v1' },
    { key: 'k2', value: 'v2\n续行内容\n残句无分隔' },
  ]);
  // 无分隔行出现在任何条目之前 → 丢弃
  const raw2 = [':::absorb-visual', '::::kv', '残句', 'k :: v', '::::', ':::'].join('\n');
  assert.deepEqual(parseVisualDocument(raw2).preamble.blocks[0].items, [{ key: 'k', value: 'v' }]);
});

test('timeline：全角/半角冒号分隔、缺标签、缩进续行', () => {
  const raw = [
    ':::absorb-visual',
    '::::timeline',
    '- SYN：客户端发送同步报文',
    '  细节补充',
    '- 1995: 标准发布',
    '- 纯描述无标签',
    '::::',
    ':::',
  ].join('\n');
  const doc = parseVisualDocument(raw);
  assert.deepEqual(doc.preamble.blocks[0].items, [
    { label: 'SYN', text: '客户端发送同步报文\n细节补充' },
    { label: '1995', text: '标准发布' },
    { label: '', text: '纯描述无标签' },
  ]);
});

test('tree：缩进嵌套与层级回退', () => {
  const raw = [':::absorb-visual', '::::tree', '- L1a', '  - L2a', '  - L2b', '    - L3', '- L1b', '::::', ':::'].join('\n');
  const doc = parseVisualDocument(raw);
  const root = doc.preamble.blocks[0].root;
  assert.equal(root.children.length, 2);
  assert.equal(root.children[0].label, 'L1a');
  assert.equal(root.children[0].children.length, 2);
  assert.equal(root.children[0].children[1].children[0].label, 'L3');
  assert.equal(root.children[1].label, 'L1b');
});

test('未知组件名 → 整块退化为 markdown（含 ::: 行原样保留）', () => {
  const raw = [':::absorb-visual', '::::mindmap', '数据', '::::', ':::'].join('\n');
  const doc = parseVisualDocument(raw);
  assert.equal(doc.preamble.blocks.length, 1);
  assert.equal(doc.preamble.blocks[0].kind, 'md');
  assert.ok(doc.preamble.blocks[0].text.includes('::::mindmap'));
});

test('容器内 mermaid 围栏保持为 md 块（不被组件扫描破坏）', () => {
  const raw = [':::absorb-visual', '## 图', '', '```mermaid', 'flowchart TD', '    A["x"] --> B["y"]', '```', ':::'].join('\n');
  const doc = parseVisualDocument(raw);
  assert.equal(doc.sections.length, 1);
  assert.equal(doc.sections[0].blocks[0].kind, 'md');
  assert.ok(doc.sections[0].blocks[0].text.includes('flowchart TD'));
});

// ---------- 鲁棒性增强（借鉴 answer-me-with-html）----------

test('BOM 与 CRLF 归一化', () => {
  const raw = '\uFEFF:::absorb-visual\r\n---\r\ntitle: T\r\n---\r\n正文\r\n:::';
  const segs = parseVisualSegments(raw);
  assert.equal(segs.length, 1);
  assert.equal(segs[0].type, 'visual');
  assert.ok(segs[0].closed);
  const doc = parseVisualDocument(segs[0].raw);
  assert.equal(doc.meta.title, 'T');
  assert.ok(doc.preamble.blocks[0].text.includes('正文'));
});

test('围栏闭合：异字符或长度不足不闭合（``` 内的 ~~~ 不算）', () => {
  // ``` 打开，~~~ 不闭合它，::: 被围栏保护
  const input = [':::absorb-visual', '```text', '~~~', ':::', '```', '内容', ':::'].join('\n');
  const segs = parseVisualSegments(input);
  assert.equal(segs.length, 1);
  assert.ok(segs[0].raw.includes('内容'));
  assert.ok(segs[0].closed);
  // `` 打开（2 反引号），``` 不闭合（长度不足），`` 才闭合
  const short = parseVisualSegments('``\nx\n```\ny\n``\n尾');
  assert.equal(short[0].text.includes('```'), true);
});

test('表格缺分隔行 → 自动补 `| --- |`', () => {
  const fixed = fixMarkdownTables('| 维度 | 跳表 |\n| 实现复杂度 | 低 |');
  const lines = fixed.split('\n');
  assert.equal(lines.length, 3);
  assert.match(lines[1], /^\| --- \| --- \|$/);
  // 已有分隔行不动
  const ok = fixMarkdownTables('| a | b |\n| --- | --- |\n| 1 | 2 |');
  assert.equal(ok.split('\n').length, 3);
  // 围栏内不误补
  const fenced = fixMarkdownTables('```\n| a | b |\n| c | d |\n```');
  assert.equal(fenced.split('\n').length, 4);
  // 正常多行数据表格：一行都不能插（线上故障：每两行之间插出 --- 行）
  const multi = fixMarkdownTables('| a | b |\n| --- | --- |\n| 1 | 2 |\n| 3 | 4 |\n| 5 | 6 |');
  const ml = multi.split('\n');
  assert.equal(ml.length, 5);
  assert.deepEqual(ml, ['| a | b |', '| --- | --- |', '| 1 | 2 |', '| 3 | 4 |', '| 5 | 6 |']);
  // 缺分隔 + 多行数据：只在表头后补一行
  const missing = fixMarkdownTables('| a | b |\n| 1 | 2 |\n| 3 | 4 |');
  const mo = missing.split('\n');
  assert.equal(mo.length, 4);
  assert.match(mo[1], /^\| --- \| --- \|$/);
  assert.equal(mo[2], '| 1 | 2 |');
  assert.equal(mo[3], '| 3 | 4 |');
});

test('错误收集：未闭合容器 / 未知组件 / tree 的 Mermaid 习惯 / kv 残句', () => {
  const segErrs = [];
  const segs = parseVisualSegments(':::absorb-visual\nA', segErrs);
  assert.equal(segs[0].closed, false);
  assert.equal(segErrs.length, 1);
  assert.equal(segErrs[0].component, 'absorb-visual');
  assert.match(segErrs[0].message, /未闭合/);
  assert.equal(segErrs[0].repaired, true); // 已按文末自动闭合，非致命（2026-11 修复）

  const docErrs = [];
  const raw = [':::absorb-visual', '## 面板', '::::mindmap', 'x', '::::', ':::'].join('\n');
  parseVisualDocument(raw, docErrs);
  assert.ok(docErrs.some((e) => e.component === 'mindmap' && /未知组件/.test(e.message)));
  assert.equal(docErrs[0].line, 3); // 容器内 1-based 行号

  const treeErrs = [];
  const rawTree = [':::absorb-visual', '::::tree', '- A[开始]', '- B(结束)', '::::', ':::'].join('\n');
  parseVisualDocument(rawTree, treeErrs);
  assert.equal(treeErrs.filter((e) => e.component === 'tree').length, 2);
  assert.ok(treeErrs[0].message.includes('A[开始]'));

  const kvErrs = [];
  const rawKv = [':::absorb-visual', '::::kv', '残句', 'k :: v', '::::', ':::'].join('\n');
  const kvDoc = parseVisualDocument(rawKv, kvErrs);
  assert.equal(kvDoc.preamble.blocks[0].items.length, 1); // 残句丢弃，kv 正常
  assert.ok(kvErrs.some((e) => e.component === 'kv' && /分隔符/.test(e.message)));
});

test('chart：meta 元信息与数据行解析', () => {
  const raw = [':::absorb-visual', '::::chart bar', 'title: 构建耗时', 'unit: 秒', 'Go :: 4.2', 'Rust :: 1.1', 'Java :: 12.8', '::::', ':::'].join('\n');
  const doc = parseVisualDocument(raw);
  const b = doc.preamble.blocks[0];
  assert.equal(b.kind, 'chart');
  assert.equal(b.chartType, 'bar');
  assert.equal(b.title, '构建耗时');
  assert.equal(b.unit, '秒');
  assert.deepEqual(b.items, [
    { label: 'Go', value: 4.2 },
    { label: 'Rust', value: 1.1 },
    { label: 'Java', value: 12.8 },
  ]);
  // 开行缺类型参数 → 默认 bar；数据开始后的 title: 行不再当元信息
  const raw2 = [':::absorb-visual', '::::chart', 'a :: 1', '::::', ':::'].join('\n');
  const doc2 = parseVisualDocument(raw2);
  assert.equal(doc2.preamble.blocks[0].chartType, 'bar');
});

test('chart：畸形行逐条丢弃并记 errors（行号对齐）', () => {
  const errs = [];
  // 容器开行 1 → body 首行（::::chart）行号 2 → 数据首行行号 3
  const raw = [':::absorb-visual', '::::chart bar', 'Go :: 10', 'Rust :: 快', '残句无分隔', 'Java :: -5', 'Hi :: 3', '::::', ':::'].join('\n');
  const doc = parseVisualDocument(raw, errs);
  const b = doc.preamble.blocks[0];
  assert.equal(b.kind, 'chart');
  assert.deepEqual(b.items, [{ label: 'Go', value: 10 }, { label: 'Hi', value: 3 }]);
  const chartErrs = errs.filter((e) => e.component === 'chart');
  assert.equal(chartErrs.length, 3);
  assert.ok(chartErrs.some((e) => e.line === 4 && /数值无法解析/.test(e.message)));
  assert.ok(chartErrs.some((e) => e.line === 5 && /分隔符/.test(e.message)));
  assert.ok(chartErrs.some((e) => e.line === 6 && /负数/.test(e.message)));
});

test('chart：零有效数据 / 未知类型 → 整块退化为原文', () => {
  const errs = [];
  const empty = parseVisualDocument([':::absorb-visual', '::::chart bar', '只有一句话', '::::', ':::'].join('\n'), errs);
  assert.equal(empty.preamble.blocks[0].kind, 'md');
  assert.ok(empty.preamble.blocks[0].text.includes('只有一句话'));
  assert.ok(errs.some((e) => e.component === 'chart' && /无有效数据/.test(e.message)));
  const errs2 = [];
  const pie = parseVisualDocument([':::absorb-visual', '::::chart pie', 'a :: 1', '::::', ':::'].join('\n'), errs2);
  assert.equal(pie.preamble.blocks[0].kind, 'md');
  assert.ok(errs2.some((e) => e.component === 'chart' && /暂不支持的图表类型/.test(e.message)));
});

// ---------- 组件写法兼容（上游主语法：围栏；冒号按名识别） ----------

test('回归：3 冒号组件开行不再让容器提前闭合（线上真实故障形状）', () => {
  const raw = [
    ':::absorb-visual',
    '---', 'title: T', '---',
    '## 膨胀与查询代价',
    ':::chart bar',
    'title: tokens 列与正文列字符数对比',
    'unit: 千字符',
    'tokens 列 :: 894',
    'content 正文 :: 615',
    ':::',
    '',
    '膨胀率 1.455×，即索引比正文本身大 45%。',
    ':::',
  ].join('\n');
  const segs = parseVisualSegments(raw);
  assert.equal(segs.length, 1);
  assert.equal(segs[0].type, 'visual');
  assert.ok(segs[0].closed);
  assert.ok(segs[0].raw.includes('膨胀率'));   // 正文不再外溢
  const doc = parseVisualDocument(segs[0].raw);
  const chart = doc.sections[0].blocks.find((b) => b.kind === 'chart');
  assert.ok(chart);
  assert.deepEqual(chart.items, [
    { label: 'tokens 列', value: 894 },
    { label: 'content 正文', value: 615 },
  ]);
  assert.ok(doc.sections[0].blocks.some((b) => b.kind === 'md' && b.text.includes('膨胀率')));
});

test('冒号组件：开闭都用 3 冒号也认（按组件名识别，不靠冒号数）', () => {
  const raw = [':::absorb-visual', ':::kv', 'a :: b', ':::', ':::'].join('\n');
  const segs = parseVisualSegments(raw);
  assert.equal(segs.length, 1);
  assert.ok(segs[0].closed);
  const doc = parseVisualDocument(segs[0].raw);
  assert.deepEqual(doc.preamble.blocks[0].items, [{ key: 'a', value: 'b' }]);
});

test('围栏组件：```chart bar 与 ```kv 等价于冒号写法（上游主语法）', () => {
  const raw = [
    ':::absorb-visual',
    '## 数字',
    '',
    '```chart bar',
    'title: 耗时',
    'Go :: 4',
    'Rust :: 1',
    '```',
    '',
    '```kv',
    'a :: b',
    '```',
    ':::',
  ].join('\n');
  const doc = parseVisualDocument(raw);
  const comps = doc.sections[0].blocks.filter((b) => b.kind !== 'md');
  assert.equal(comps.length, 2);
  assert.equal(comps[0].kind, 'chart');
  assert.equal(comps[0].title, '耗时');
  assert.deepEqual(comps[0].items, [{ label: 'Go', value: 4 }, { label: 'Rust', value: 1 }]);
  assert.equal(comps[1].kind, 'kv');
  assert.deepEqual(comps[1].items, [{ key: 'a', value: 'b' }]);
});

test('围栏非组件语言仍是 md（mermaid / 代码块不被误识别）', () => {
  const raw = [
    ':::absorb-visual',
    '```mermaid',
    'flowchart TD',
    '  A --> B',
    '```',
    '',
    '```js',
    'const a = 1;',
    '```',
    ':::',
  ].join('\n');
  const doc = parseVisualDocument(raw);
  assert.ok(doc.preamble.blocks.every((b) => b.kind === 'md'));
  const text = doc.preamble.blocks.map((b) => b.text).join('\n');
  assert.ok(text.includes('flowchart TD'));
  assert.ok(text.includes('const a = 1;'));
});

test('围栏组件闭合要求同字符且长度不小于开行', () => {
  const raw = [':::absorb-visual', '````chart bar', 'a :: 1', '```', 'b :: 2', '````', ':::'].join('\n');
  const doc = parseVisualDocument(raw);
  const chart = doc.preamble.blocks.find((b) => b.kind === 'chart');
  assert.ok(chart);
  assert.deepEqual(chart.items.map((it) => it.label), ['a', 'b']);
});

test('未知组件 :::note 保持原文，且不破坏容器闭合', () => {
  const errs = [];
  const raw = [':::absorb-visual', ':::note', '备注', ':::', '正文', ':::'].join('\n');
  const segs = parseVisualSegments(raw, errs);
  assert.equal(segs.length, 1);
  assert.ok(segs[0].closed);
  const doc = parseVisualDocument(segs[0].raw, errs);
  const dumped = JSON.stringify(doc);
  assert.ok(dumped.includes(':::note'));
  assert.ok(dumped.includes('正文'));
  assert.ok(errs.some((e) => e.component === 'note' && /未知组件/.test(e.message)));
});

// ---------- SPEC 契约守护：把文档里可机械校验的条款钉成测试 ----------
// 每条对应 SPEC.md 的一节；文档改了而代码没跟上时，这些用例会红。

test('SPEC 围栏写法：体内含代码块时用 4 反引号包裹，组件不被截断', () => {
  const raw = [':::absorb-visual', '````callout tip', '示例代码：', '```js', 'const a = 1;', '```', '````', ':::'].join('\n');
  const doc = parseVisualDocument(raw, []);
  const c = doc.preamble.blocks.find((b) => b.kind === 'callout');
  assert.ok(c, 'callout 应正常解析（文档：体内代码块需 4 反引号）');
  assert.ok(c.text.includes('```js'));
  assert.ok(c.text.includes('const a = 1;'));
});

test('SPEC 表格自愈：callout 体内补分隔行，代码块围栏内不补', () => {
  const inCallout = parseVisualDocument([':::absorb-visual', '```callout tip', '| a | b |', '| 1 | 2 |', '```', ':::'].join('\n'));
  assert.match(inCallout.preamble.blocks[0].text, /\| --- \| --- \|/);
  const inCode = parseVisualDocument([':::absorb-visual', '```js', '| a | b |', '| 1 | 2 |', '```', ':::'].join('\n'));
  assert.ok(!inCode.preamble.blocks[0].text.includes('| --- |'));
});

test('SPEC 错误契约：三字段齐全，parseVisualSegments 用全文行号', () => {
  const errs = [];
  // 容器开行在第 3 行（前面有正文与空行），未闭合
  parseVisualSegments('前言\n\n:::absorb-visual\nA', errs);
  assert.equal(errs.length, 1);
  // 基础三字段 + 可选 repaired 标记（自愈类错误专用，见 SPEC「到文末未闭合」）
  assert.deepEqual(
    Object.keys(errs[0]).sort(),
    ['component', 'line', 'message', 'repaired'],
  );
  assert.equal(errs[0].line, 3);
  assert.equal(errs[0].component, 'absorb-visual');
});

test('SPEC 冒号写法：4 冒号开行可被 3 冒号裸行闭合', () => {
  const raw = [':::absorb-visual', '::::kv', 'a :: b', ':::', ':::'].join('\n');
  const segs = parseVisualSegments(raw);
  assert.equal(segs.length, 1);
  assert.ok(segs[0].closed);
  const doc = parseVisualDocument(segs[0].raw);
  assert.deepEqual(doc.preamble.blocks[0].items, [{ key: 'a', value: 'b' }]);
});

// ---------- 隐式容器：全文无容器但出现组件围栏 ----------

test('隐式容器：无容器 + ```tree → 提升为可视化段并可解析成 tree', () => {
  const errs = [];
  const segs = parseVisualSegments(['前言', '', '```tree', '- A', '  - B', '```', '', '后记'].join('\n'), errs);
  assert.deepEqual(segs.map((s) => s.type), ['md', 'visual', 'md']);
  assert.equal(segs[1].implicit, true);
  assert.equal(segs[1].closed, true);
  assert.ok(segs[1].raw.startsWith(':::absorb-visual'));
  assert.ok(segs[1].raw.includes('```tree'));
  assert.ok(segs[2].text.includes('后记'));
  const doc = parseVisualDocument(segs[1].raw);
  assert.equal(doc.preamble.blocks[0].kind, 'tree');
  assert.equal(doc.preamble.blocks[0].root.children[0].label, 'A');
  assert.ok(errs.some((e) => /隐式容器/.test(e.message)));
  assert.equal(errs[0].line, 3); // 围栏开行的全文行号
});

test('隐式容器：无容器且无组件围栏 → 行为不变（单个 md 段）', () => {
  const segs = parseVisualSegments('一段普通回答\n\n- 列表');
  assert.equal(segs.length, 1);
  assert.equal(segs[0].type, 'md');
  assert.deepEqual(parseVisualSegments(''), []);
});

test('隐式容器：有容器时不提升容器外的组件围栏（保持 md）', () => {
  const input = [':::absorb-visual', '## 面板', '正文', ':::', '', '```tree', '- A', '```'].join('\n');
  const segs = parseVisualSegments(input);
  const md = segs.find((s) => s.type === 'md');
  assert.ok(md.text.includes('```tree'));
  assert.equal(segs.filter((s) => s.type === 'visual').length, 1);
});

test('隐式容器：非组件语言（js）不被提升', () => {
  const segs = parseVisualSegments(['```js', 'const a = 1;', '```'].join('\n'));
  assert.equal(segs.length, 1);
  assert.equal(segs[0].type, 'md');
});

test('隐式容器：多个组件块各自成段，md 顺序保持', () => {
  const input = ['说明', '', '```kv', 'a :: b', '```', '', '中间', '', '```callout tip', '提示', '```'].join('\n');
  const segs = parseVisualSegments(input);
  assert.deepEqual(segs.map((s) => s.type), ['md', 'visual', 'md', 'visual']);
  assert.ok(segs[2].text.includes('中间'));
});

test('恶意输入批：不抛异常、产物安全', () => {
  const hostile = [
    '', null, undefined, 42, {}, '|||', ':::::', '::::::kv', '\u0000',
    'x'.repeat(3000), '🚀'.repeat(500), '| a | b |', '\r\r\n\n', '::::'.repeat(100),
    ':::absorb-visual\n' + ':::'.repeat(50), '```' + '\n:::absorb-visual',
  ];
  for (const input of hostile) {
    const errs = [];
    assert.doesNotThrow(() => parseVisualSegments(input, errs));
    assert.doesNotThrow(() => parseVisualDocument(input, errs));
    for (const seg of parseVisualSegments(input, errs)) {
      if (seg.type === 'visual') assert.doesNotThrow(() => parseVisualDocument(seg.raw, errs));
    }
    assert.ok(errs.every((e) => typeof e.message === 'string' && e.component));
  }
});
