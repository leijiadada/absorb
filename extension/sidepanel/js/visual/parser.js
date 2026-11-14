// Absorb Visual Explanation - DSL v1 解析器
// 零依赖纯函数：不引用 DOM、不 import 任何 Absorb 模块，可整体拷走复用。
// 职责：① 从回答全文切分出 :::absorb-visual 块（parseVisualSegments）
//      ② 把块内原文解析为结构化文档（parseVisualDocument：frontmatter + 前导 + 分节 + 组件）
// 语法契约见同目录 SPEC.md。解析失败不抛异常的关键路径只有一条原则：
// 外层容器缺失 → 不产生 visual 段；内层组件畸形 → 该组件退化为 markdown 块。
// 错误策略（借鉴 answer-me-with-html 的错误收集）：全程收集而非 fail-fast——
// 各解析入口可传 errors 数组，收 {line, component, message}（line 为该层级内 1-based
// 行号），调用方决定展示/上报；不传则静默（兼容既有调用）。

// ---------- 输入归一化 ----------

// BOM 剥除 + 换行归一（CRLF / 孤立 CR → LF）。SSE 流与剪贴板来源都可能出现。
function normalizeInput(content) {
  return String(content || '').replace(/^\uFEFF/, '').replace(/\r\n?/g, '\n');
}

// ---------- 行级工具 ----------

// 围栏开关行信息：返回 {char:'`'|'~', len} 或 null（允许 ≤3 空格缩进）。
// 围栏内一切冒号规则失效；闭合必须**同字符**且**长度 ≥ 开行**（``` 内的 ~~~ 不是闭合）。
function fenceOf(line) {
  const m = line.match(/^ {0,3}(`{3,}|~{3,})/);
  return m ? { char: m[1][0], len: m[1].length } : null;
}

// 围栏开行信息：返回 {char, len, lang, args} 或 null。lang 为围栏后的首个 token
// （```chart bar → lang='chart', args='bar'），用于识别"围栏组件"写法（上游主语法）。
function fenceInfo(line) {
  const m = line.match(/^ {0,3}(`{3,}|~{3,}) *([^\s`]*)(.*)\r?$/);
  if (!m) return null;
  return { char: m[1][0], len: m[1].length, lang: (m[2] || '').toLowerCase(), args: (m[3] || '').trim() };
}

// 是否"裸冒号行"：只由冒号（允许尾随空白）组成的行，返回冒号数；否则 0。
function bareColonCount(line) {
  const m = line.match(/^ {0,3}(:+) *\r?$/);
  return m ? m[1].length : 0;
}

// 组件白名单：围栏写法的语言名与冒号写法的名称都以此判定（未知组件退化为原文）。
const COMPONENT_NAMES = new Set(['callout', 'kv', 'timeline', 'tree', 'chart']);

// 外层容器开行：`:::absorb-visual`（≥3 冒号 + 名称，忽略大小写与首尾空白）。
function matchContainerOpen(line) {
  const m = line.match(/^ {0,3}(:{3,}) *absorb-visual *\r?$/i);
  return m ? m[1].length : 0;
}

// 组件开行：`:::name [参数...]`（≥3 冒号，名称小写字母）。
// 与上游一致：**按组件名识别**，不靠冒号数区分层级——模型写 :::chart 或 ::::chart 都认。
// 容器名 absorb-visual 归 matchContainerOpen，这里排除，避免 `::: absorb-visual` 误判。
function matchComponentOpen(line) {
  if (matchContainerOpen(line) > 0) return null;
  const m = line.match(/^ {0,3}(:{3,}) *([a-z][a-z0-9_-]*) *(.*)\r?$/i);
  if (!m) return null;
  return { colons: m[1].length, name: m[2].toLowerCase(), rest: m[3].trim() };
}

// ---------- ① 分段：从全文切出 absorb-visual 块 ----------

// 返回 [{type:'md', text} | {type:'visual', raw, closed}]
// raw 为容器开行（含）到闭合行（含）之间的原文（不含闭合行）。
// 闭合规则：裸冒号数与开行**相等**的裸冒号行（fence 内不生效）——组件用 4 冒号开闭，
// 不会被外层误判为闭合。到文末未闭合 → closed:false（仍交由渲染层宽容渲染），
// 并向 errors（若提供）记一条错误。
// errors（可选）：收集 {line, component, message}，line 为全文 1-based 行号。
export function parseVisualSegments(content, errors) {
  const lines = normalizeInput(content).split('\n');
  // 隐式容器兜底：整篇没有任何 :::absorb-visual 容器、却出现组件围栏时启用
  // （线上故障：模型写对了 ```tree 但漏开容器，组件静默退化成代码块）。
  // 仅在"全文无容器"时生效——已有容器说明格式对了，不再猜测，避免误提升示例代码。
  if (!hasAnyContainer(lines)) return parseImplicitComponents(lines, errors);
  const segs = [];
  const pushMd = (from, to) => {
    const text = lines.slice(from, to).join('\n');
    if (text !== '') segs.push({ type: 'md', text });
  };
  let mdStart = 0; // 当前未入段的 md 起始行号
  let fence = null; // {char, len} | null
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    const f = fenceOf(line);
    if (fence) {
      if (f && f.char === fence.char && f.len >= fence.len) fence = null;
      i++;
      continue;
    }
    if (f) { fence = f; i++; continue; }
    const open = matchContainerOpen(line);
    if (open > 0) {
      if (i > mdStart) pushMd(mdStart, i);
      // 收集到闭合行（顶层裸冒号数 == 开行）或文末。
      // 就近闭合：容器体内一旦有组件开行（:::name，含 3 冒号），裸冒号行先闭组件，
      // 只有顶层（无组件打开）的裸冒号行才闭容器——避免组件的闭合行把容器提前关掉
      // （曾导致正文外溢 / 末尾出现孤儿 :::，见 SPEC.md 闭合规则）。
      let j = i + 1;
      let closed = false;
      let comp = 0; // 当前打开的组件冒号数（0 = 无）
      while (j < lines.length) {
        const fj = fenceOf(lines[j]);
        if (fence) {
          if (fj && fj.char === fence.char && fj.len >= fence.len) fence = null;
          j++;
          continue;
        }
        if (fj) { fence = fj; j++; continue; }
        if (comp) {
          // 组件体内：任意裸冒号行（≥3）闭合组件，冒号数不要求与开行相同
          if (bareColonCount(lines[j]) >= 3) comp = 0;
          j++;
          continue;
        }
        const cOpen = matchComponentOpen(lines[j]);
        if (cOpen) { comp = cOpen.colons; j++; continue; }
        if (bareColonCount(lines[j]) === open) { closed = true; break; }
        j++;
      }
      if (!closed && errors) {
        // repaired 标记：该偏差已自动按文末闭合修复，渲染结果与显式闭合一致（非致命）。
        // 仍记入 errors 留痕——语法纪律统计与排查需要知道模型漏了闭合行。
        errors.push({ line: i + 1, component: 'absorb-visual', repaired: true, message: '容器未闭合（缺少等数冒号闭合行），已按文末自动闭合渲染' });
      }
      // raw 含开行、不含闭合行；closed=false 时 raw 到文末
      segs.push({ type: 'visual', raw: lines.slice(i, closed ? j : lines.length).join('\n'), closed });
      i = closed ? j + 1 : lines.length;
      mdStart = i;
      continue;
    }
    i++;
  }
  if (mdStart < lines.length) pushMd(mdStart, lines.length);
  return segs;
}

// 全文是否出现过容器开行。围栏内不算——代码块里演示 `:::absorb-visual` 不应改变行为。
function hasAnyContainer(lines) {
  let fence = null;
  for (const line of lines) {
    const f = fenceOf(line);
    if (fence) {
      if (f && f.char === fence.char && f.len >= fence.len) fence = null;
      continue;
    }
    if (f) { fence = f; continue; }
    if (matchContainerOpen(line) > 0) return true;
  }
  return false;
}

// 隐式容器合成的容器头（最小 frontmatter，组件直接落在导语区）
const IMPLICIT_WRAP = [':::absorb-visual', '---', 'template: sheet', '---'];

// 隐式容器：全文没有容器时，把顶层的组件围栏块各自包成一个可视化段，
// 其余内容保持 md（顺序不变）。合成 raw 后交给 parseVisualDocument 走正常组件解析。
// 未闭合的组件围栏按"到文末"处理，仍合成闭合段——由组件层做宽容解析。
function parseImplicitComponents(lines, errors) {
  const segs = [];
  let mdStart = 0;
  let i = 0;
  while (i < lines.length) {
    const fi = fenceInfo(lines[i]);
    if (fi && COMPONENT_NAMES.has(fi.lang)) {
      let j = i + 1;
      let closed = false;
      while (j < lines.length) {
        const fj = fenceOf(lines[j]);
        if (fj && fj.char === fi.char && fj.len >= fi.len) { closed = true; break; }
        j++;
      }
      if (i > mdStart) {
        const text = lines.slice(mdStart, i).join('\n');
        if (text !== '') segs.push({ type: 'md', text });
      }
      const block = lines.slice(i, closed ? j + 1 : lines.length);
      if (errors) {
        errors.push({ line: i + 1, component: fi.lang, repaired: true, message: '缺少 :::absorb-visual 容器，已按隐式容器渲染该组件' });
      }
      segs.push({
        type: 'visual',
        raw: [...IMPLICIT_WRAP, ...block, ':::'].join('\n'),
        closed: true,
        implicit: true,
      });
      i = closed ? j + 1 : lines.length;
      mdStart = i;
      continue;
    }
    i++;
  }
  if (mdStart < lines.length) {
    const text = lines.slice(mdStart).join('\n');
    if (text !== '') segs.push({ type: 'md', text });
  }
  return segs;
}

// ---------- frontmatter（简单 key: value，不支持嵌套结构） ----------

// frontmatter 键白名单（version 为历史兼容，SPEC 只承诺三个键）。
const FRONTMATTER_KEY = /^(template|title|subtitle|version)\s*:/i;

// 键行串消费（frontmatter 自愈）：从 from 起连续识别元信息键行写入 meta，
// 首个非键行即正文起点。修复 2026-11 线上泄漏：`---` 不成对或整体缺失时，
// `template:/title:/subtitle:` 行原本整段漏成可见正文。至少消费到一个键行才生效，
// 否则原文返回（普通正文以 "title: " 开头等罕见形状不受影响）。
function consumeFrontmatterKeyRun(lines, from, meta) {
  let i = from;
  let consumed = 0;
  for (; i < lines.length; i++) {
    const line = lines[i];
    if (line.trim() === '') continue;
    if (!FRONTMATTER_KEY.test(line)) break;
    const m = line.match(/^([A-Za-z_][\w-]*) *: *(.*)\r?$/);
    if (!m) break;
    const key = m[1].toLowerCase();
    const val = m[2].trim().replace(/^["']|["']$/g, '');
    if (key in meta && val !== '') meta[key] = key === 'version' ? (parseInt(val, 10) || 1) : val;
    consumed++;
  }
  return lines.slice(consumed > 0 ? i : from).join('\n');
}

function parseFrontmatter(raw) {
  const meta = { version: 1, template: 'sheet', title: '', subtitle: '' };
  const lines = String(raw || '').split('\n');
  let i = 0;
  while (i < lines.length && lines[i].trim() === '') i++;
  if ((lines[i] || '').trim() !== '---') {
    // 自愈路径 A：整体漏写 ---，首部键行串仍识别为元信息
    return { meta, rest: consumeFrontmatterKeyRun(lines, i, meta) };
  }
  i++;
  const fmStart = i; // 开行 --- 之后（闭合扫描会推进 i，自愈需从键行首重新起算）
  let end = -1;
  for (; i < lines.length; i++) {
    if (lines[i].trim() === '---') { end = i; break; }
  }
  if (end === -1) {
    // 自愈路径 B：--- 不成对（漏写闭合行），开行后的键行串识别为元信息
    return { meta, rest: consumeFrontmatterKeyRun(lines, fmStart, meta) };
  }
  for (let k = 0; k < end; k++) {
    const m = lines[k].match(/^([A-Za-z_][\w-]*) *: *(.*)\r?$/);
    if (!m) continue;
    const key = m[1].toLowerCase();
    const val = m[2].trim().replace(/^["']|["']$/g, '');
    if (key in meta && val !== '') meta[key] = key === 'version' ? (parseInt(val, 10) || 1) : val;
  }
  return { meta, rest: lines.slice(end + 1).join('\n') };
}

// ---------- ② 组件块扫描（容器体内） ----------

// 在正文行中切分组件块：返回 [{kind:'md', text} | 组件块]
// 组件闭行：裸冒号数 ≥ 组件开行冒号数（fence 内不生效）。
// 名称不在白名单 → 整块按 markdown 保留（含 ::: 行原样交给 marked，退化可读），
// 并向 errors 记一条（baseLine 为 lines[0] 在文档中的 1-based 行号，默认 1）。
function scanComponents(lines, errors, baseLine = 1) {
  const blocks = [];
  let mdStart = 0;
  let fence = null;
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    // 围栏组件写法（上游主语法）：```chart bar … ```，与冒号写法完全等价。
    // 语言名不在组件白名单时走普通围栏（mermaid / 代码块），保持为 md 块。
    const fi = fenceInfo(line);
    if (fi && COMPONENT_NAMES.has(fi.lang)) {
      if (i > mdStart) blocks.push({ kind: 'md', text: fixMarkdownTables(lines.slice(mdStart, i).join('\n')) });
      let j = i + 1;
      let closed = false;
      while (j < lines.length) {
        const fj = fenceOf(lines[j]);
        if (fj && fj.char === fi.char && fj.len >= fi.len) { closed = true; break; }
        j++;
      }
      blocks.push(buildComponent(
        { name: fi.lang, rest: fi.args, colons: fi.len },
        lines.slice(i + 1, closed ? j : lines.length),
        errors, baseLine + i + 1
      ));
      i = closed ? j + 1 : lines.length;
      mdStart = i;
      continue;
    }
    const f = fenceOf(line);
    if (fence) {
      if (f && f.char === fence.char && f.len >= fence.len) fence = null;
      i++; continue;
    }
    if (f) { fence = f; i++; continue; }
    const open = matchComponentOpen(line);
    if (open && !COMPONENT_NAMES.has(open.name) && errors) {
      errors.push({ line: baseLine + i, component: open.name, message: '未知组件，整块按原文保留' });
    }
    if (open && COMPONENT_NAMES.has(open.name)) {
      if (i > mdStart) blocks.push({ kind: 'md', text: fixMarkdownTables(lines.slice(mdStart, i).join('\n')) });
      let j = i + 1;
      let closed = false;
      while (j < lines.length) {
        const fj = fenceOf(lines[j]);
        if (fence) {
          if (fj && fj.char === fence.char && fj.len >= fence.len) fence = null;
          j++; continue;
        }
        if (fj) { fence = fj; j++; continue; }
        // 就近闭合：任意裸冒号行（≥3）闭组件，冒号数不要求与开行相同
        if (bareColonCount(lines[j]) >= 3) { closed = true; break; }
        j++;
      }
      blocks.push(buildComponent(open, lines.slice(i + 1, closed ? j : lines.length), errors, baseLine + i + 1));
      i = closed ? j + 1 : lines.length;
      mdStart = i;
      continue;
    }
    i++;
  }
  if (mdStart < lines.length) blocks.push({ kind: 'md', text: fixMarkdownTables(lines.slice(mdStart).join('\n')) });
  return blocks;
}

// 表格分隔行自动修复（借鉴 answer-me-with-html checkTables）：
// LLM 高频失误是表格漏写 `| --- |` 分隔行——marked 不识别为表格。检测"表头行 +
// 紧跟另一个 | 行但后者不是分隔行"则在两者之间插入自动生成的分隔行。
// 只认**表格首行**（上一行不是表格行）：否则正常多行表格的每两行之间都会被插一条
// `| --- |`（线上故障，截图可见表格里多出 --- 行）。
// 围栏内不处理；列数按未转义 | 计数（\| 转义的表格少见，修复场景以简单表格为主）。
export function fixMarkdownTables(text) {
  const lines = String(text || '').split('\n');
  const isRow = (s) => /^\s*\|.+\|?\s*$/.test(s);
  const isSep = (s) => /^\s*\|?[\s:|-]+\|?\s*$/.test(s) && /-/.test(s);
  const colCount = (s) => s.trim().replace(/^\|/, '').replace(/\|$/, '').split('|').length;
  const out = [];
  let fence = null;
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    const f = fenceOf(line);
    if (fence) {
      out.push(line);
      if (f && f.char === fence.char && f.len >= fence.len) fence = null;
      continue;
    }
    if (f) { fence = f; out.push(line); continue; }
    if (isRow(line) && !isSep(line) && !(i > 0 && isRow(lines[i - 1])) &&
        i + 1 < lines.length && isRow(lines[i + 1]) && !isSep(lines[i + 1])) {
      out.push(line);
      out.push('| ' + Array(colCount(line)).fill('---').join(' | ') + ' |');
      continue;
    }
    out.push(line);
  }
  return out.join('\n');
}

function buildComponent(open, bodyLines, errors, line) {
  const body = bodyLines.join('\n');
  switch (open.name) {
    case 'callout': {
      // `::::callout [tip|warn|info]`，body 为 markdown（含表格修复）
      const variant = ['tip', 'warn', 'info'].includes(open.rest.toLowerCase()) ? open.rest.toLowerCase() : 'info';
      return { kind: 'callout', variant, text: fixMarkdownTables(body) };
    }
    case 'kv':
      return { kind: 'kv', items: parseKvLines(bodyLines, errors, line) };
    case 'timeline':
      return { kind: 'timeline', items: parseTimelineLines(bodyLines) };
    case 'tree':
      return { kind: 'tree', root: parseTreeLines(bodyLines, errors, line) };
    case 'chart': {
      // `::::chart [bar]`——V1 仅 bar；数据行 `名称 :: 数字`，首部可选 title/unit 元信息。
      // 降级纪律见 SPEC.md：零条有效数据或类型不支持 → 整块退化为原文。
      const type = (open.rest || 'bar').toLowerCase();
      if (type !== 'bar') {
        if (errors) errors.push({ line, component: 'chart', message: `暂不支持的图表类型 "${open.rest}"，整块退化为原文` });
        return { kind: 'md', text: body };
      }
      const { meta, items } = parseChartLines(bodyLines, errors, line);
      if (!items.length) {
        if (errors) errors.push({ line, component: 'chart', message: '无有效数据行，整块退化为原文' });
        return { kind: 'md', text: body };
      }
      return { kind: 'chart', chartType: 'bar', title: meta.title, unit: meta.unit, items };
    }
    default:
      return { kind: 'md', text: body };
  }
}

// kv 行：`key :: value`（首个 ` :: ` 分隔）；无分隔符的行追加到上一条 value（多行值）。
// line 为 body 首行在文档中的 1-based 行号（错误定位用）。
function parseKvLines(lines, errors, line) {
  const items = [];
  for (let idx = 0; idx < lines.length; idx++) {
    const row = lines[idx].replace(/\r$/, '');
    if (!row.trim()) continue;
    const idx1 = idx + 1;
    const at = line + idx1 - 1;
    const i2 = row.indexOf(' :: ');
    if (i2 > 0) {
      items.push({ key: row.slice(0, i2).trim(), value: row.slice(i2 + 4).trim() });
    } else if (items.length > 0) {
      items[items.length - 1].value += '\n' + row.trim();
    } else if (errors) {
      // 首条就是无分隔行的残句：丢弃（畸形输入，宁缺勿错），并留痕
      errors.push({ line: at, component: 'kv', message: `缺少 " :: " 分隔符的行已丢弃："${row.trim().slice(0, 20)}"` });
    }
  }
  return items;
}

// timeline 行：`- 标签：描述`（首个全角"："或半角": "分隔；缺标签 → 标签为空）。
// 条目下的缩进续行并入描述。
function parseTimelineLines(lines) {
  const items = [];
  for (const raw of lines) {
    const line = raw.replace(/\r$/, '');
    if (!line.trim()) continue;
    const m = line.match(/^\s*[-*] +(.*)$/);
    if (m) {
      const text = m[1];
      const sep = text.match(/^(.*?)[:：] *(.*)$/);
      if (sep && sep[1]) {
        items.push({ label: sep[1].trim(), text: sep[2].trim() });
      } else {
        items.push({ label: '', text: text.trim() });
      }
    } else if (items.length > 0) {
      items[items.length - 1].text += (items[items.length - 1].text ? '\n' : '') + line.trim();
    }
  }
  return items;
}

// tree 行：缩进层级（2 空格 = 1 层，tab = 1 层），`- `/`* ` 前缀剥除，纯文本行同样成节点。
// 检测 Mermaid 习惯写法（`A[标签]` / `A(标签)`）→ 记入 errors 提示改写（借鉴上游 checkMermaidIds）。
function parseTreeLines(lines, errors, line) {
  const root = { label: '', text: '', children: [] };
  const stack = [{ indent: -1, node: root }];
  let mermaidHits = 0;
  for (let idx = 0; idx < lines.length; idx++) {
    const row = lines[idx].replace(/\r$/, '').replace(/\t/g, '  ');
    if (!row.trim()) continue;
    const indent = row.match(/^ */)[0].length;
    const label = row.slice(indent).replace(/^[-*] +/, '').trim();
    if (!label) continue;
    if (errors && mermaidHits < 3 && /^[A-Za-z0-9_]+[[(].+[)\]]$/.test(label)) {
      mermaidHits++;
      errors.push({ line: line + idx, component: 'tree', message: `Mermaid 习惯写法 "${label.slice(0, 20)}"，tree 组件直接写纯文本标签即可` });
    }
    const node = { label, text: '', children: [] };
    while (stack.length > 1 && stack[stack.length - 1].indent >= indent) stack.pop();
    stack[stack.length - 1].node.children.push(node);
    stack.push({ indent, node });
  }
  return root;
}

// chart 行解析：首部 `title: X` / `unit: X` 元信息（仅数据行开始前识别，单冒号），
// 数据行 `名称 :: 数字`（与 kv 同款分隔）。畸形行逐条丢弃并留痕，不中断其余行。
// line 为 body 首行在文档中的 1-based 行号（错误定位用）。
function parseChartLines(lines, errors, line) {
  const meta = { title: '', unit: '' };
  const items = [];
  let dataStarted = false;
  for (let idx = 0; idx < lines.length; idx++) {
    const row = lines[idx].replace(/\r$/, '');
    if (!row.trim()) continue;
    const at = line + idx;
    const i2 = row.indexOf(' :: ');
    if (i2 > 0) {
      dataStarted = true;
      const label = row.slice(0, i2).trim();
      const num = Number(row.slice(i2 + 4).trim().replace(/^["']|["']$/g, '').replace(/,/g, ''));
      if (!Number.isFinite(num)) {
        if (errors) errors.push({ line: at, component: 'chart', message: `数值无法解析，该行已丢弃："${label.slice(0, 20)} :: ${row.slice(i2 + 4).trim().slice(0, 12)}"` });
      } else if (num < 0) {
        if (errors) errors.push({ line: at, component: 'chart', message: `负数超出条形图语义，该行已丢弃："${label.slice(0, 20)}"` });
      } else {
        items.push({ label, value: num });
      }
    } else if (!dataStarted) {
      const m = row.match(/^(title|unit) *: *(.*)$/i);
      if (m) {
        meta[m[1].toLowerCase()] = m[2].trim().replace(/^["']|["']$/g, '');
      } else if (errors) {
        errors.push({ line: at, component: 'chart', message: `无法识别的行已丢弃："${row.trim().slice(0, 20)}"` });
      }
    } else if (errors) {
      errors.push({ line: at, component: 'chart', message: `数据行缺少 " :: " 分隔符，已丢弃："${row.trim().slice(0, 20)}"` });
    }
  }
  return { meta, items };
}

// ---------- 文档级解析 ----------

// 解析容器 raw（parseVisualSegments 的 visual 段）：
// → { meta, closed, preamble:{blocks}, sections:[{title, blocks}] }
// preamble = 第一个 `## ` 之前的内容；`## X` 开新节（### 及更深层保留在节内 markdown）。
// errors（可选）：收集 {line, component, message}，line 为容器内 1-based 行号。
export function parseVisualDocument(raw, errors) {
  const lines = normalizeInput(raw).split('\n');
  // 去掉首行容器开行与文末闭合行（调用方传入的 raw 含开行）
  let start = 0;
  while (start < lines.length && lines[start].trim() === '') start++;
  if (start < lines.length && matchContainerOpen(lines[start]) > 0) start++;
  let end = lines.length;
  while (end > start && lines[end - 1].trim() === '') end--;
  if (end > start && bareColonCount(lines[end - 1]) >= 3) end--;
  const { meta, rest } = parseFrontmatter(lines.slice(start, end).join('\n'));
  const bodyLines = rest.split('\n');
  // rest 在容器内的起始 1-based 行号（frontmatter 消耗了若干行），错误定位用
  const bodyBase = start + 1 + (lines.slice(start, end).length - bodyLines.length);

  const preambleBlocks = [];
  const sections = [];
  let fence = null;
  let curTitle = null; // null = 尚在 preamble
  let curStart = 0;
  for (let i = 0; i <= bodyLines.length; i++) {
    const line = bodyLines[i] || '';
    const f = fenceOf(line);
    if (fence) {
      if (f && f.char === fence.char && f.len >= fence.len) fence = null;
      continue;
    }
    if (f) { fence = f; continue; }
    const h = line.match(/^ {0,3}## +(?!#)(.*)\r?$/);
    if (h) {
      const chunk = bodyLines.slice(curStart, i);
      if (curTitle === null) preambleBlocks.push(...scanComponents(chunk, errors, bodyBase + curStart));
      else sections.push({ title: curTitle, blocks: scanComponents(chunk, errors, bodyBase + curStart) });
      curTitle = h[1].trim();
      curStart = i + 1;
    }
  }
  const tail = bodyLines.slice(curStart);
  if (curTitle === null) preambleBlocks.push(...scanComponents(tail, errors, bodyBase + curStart));
  else sections.push({ title: curTitle, blocks: scanComponents(tail, errors, bodyBase + curStart) });

  return { meta, preamble: { blocks: preambleBlocks }, sections };
}
