// Absorb 阅读助手 - 划词整页 HTML → 结构化文本（pageMd）转换器
// 与 Go 端 tool/pageretriever/pageextract.go 的 ExtractPageText 逐行等价（探针实测 5/5 逐字符同构），
// 产出直接作为 Go 端页面缓存内容（handler 免转换直读，见 internal/chat/handler.go）。
// 执行环境：
//   - executeScript 注入的页面隔离世界（经典脚本，挂 globalThis.__absorbPageConvert）
//   - vitest（jsdom，ESM import 后读同一全局）
// 复杂度边界：HTML 解析交给浏览器原生 DOMParser（WHATWG 实现），本文件只做树遍历渲染。

(function (factory) {
  var api = factory();
  if (typeof module !== 'undefined' && module.exports) {
    module.exports = api; // Node/CJS
  } else {
    (typeof globalThis !== 'undefined' ? globalThis : self).__absorbPageConvert = api;
  }
})(function () {
  'use strict';

  // 块级元素：渲染时强制换行分隔（标题/pre 单独处理）——与 Go isBlockTag 同表
  var BLOCK = ['p', 'div', 'li', 'ul', 'ol', 'dl', 'dt', 'dd', 'table', 'thead', 'tbody',
    'tr', 'td', 'th', 'section', 'article', 'aside', 'header', 'footer', 'main',
    'nav', 'blockquote', 'figure', 'figcaption', 'address', 'form', 'fieldset',
    'hr', 'details', 'summary', 'button', 'select', 'option', 'video', 'audio'];
  var BLOCK_SET = new Set(BLOCK);
  // 不可见/无文本语义元素——与 Go renderNode 的 skip 清单同表
  var SKIP = new Set(['script', 'style', 'noscript', 'template', 'svg', 'iframe', 'canvas', 'object']);

  // ---------- 图片占位（与 Go pageextract.go emitImage 同构） ----------
  // 网页图片在纯文本管线中原本被静默丢弃；此处产出 Markdown 图片占位行
  // ![alt|宽x高](src)，让模型感知图片的存在与主题（视觉识别走 images 数组，不在本层）。
  var IMG_MAX_LINES = 20;   // 单页占位行上限：防图片密集页挤占正文预算
  var IMG_LABEL_MAX = 60;   // 占位标签（alt）长度上限
  var IMG_SRC_MAX = 300;    // 占位 URL 长度上限
  // 装饰图启发式：无 alt 且路径命中图标/占位关键词 → 跳过（与 Go decorImgRe 同表）
  var DECOR_RE = /(icon|logo|sprite|spacer)/i;

  // 占位标签：alt → title → 文件名（src 末段）→「图片」；宽高属性齐全时追加 |宽x高
  function imgLabel(n, src) {
    var label = compactSpaces(n.getAttribute('alt') || '').trim();
    if (!label) label = compactSpaces(n.getAttribute('title') || '').trim();
    if (!label) {
      var name = src.split('?')[0].split('#')[0].split('/').pop() || '';
      label = compactSpaces(name).trim();
    }
    if (!label) label = '图片';
    if (label.length > IMG_LABEL_MAX) label = label.slice(0, IMG_LABEL_MAX) + '…';
    var w = attrNum(n, 'width', 0), h = attrNum(n, 'height', 0);
    if (w > 0 && h > 0) label += '|' + w + 'x' + h;
    return label;
  }

  // emitImage 输出单张图片占位。src 优先级 data-src → data-original → src（懒加载
  // 页的 src 常是占位图如 loading.gif，真实地址在 data-src 属性中，随 outerHTML
  // 保留）；data-uri / javascript: 跳过（防超长与脚本向量）；无 alt 的装饰图跳过；
  // 超过 IMG_MAX_LINES 只计数不输出（渲染收尾补省略行）。
  // 已知边界：label/src 含 ']' 或 ')' 时不转义——Markdown 严格解析会截断占位，
  // LLM 宽容读取下可接受，转义收益低于实现复杂度。
  function emitImage(n, b, st) {
    if (st.imgEmitted >= IMG_MAX_LINES) {
      st.imgOmitted++;
      return;
    }
    var src = (n.getAttribute('data-src') || n.getAttribute('data-original') ||
      n.getAttribute('src') || '').trim();
    if (!src || src.lastIndexOf('data:', 0) === 0 || src.lastIndexOf('javascript:', 0) === 0) return;
    var alt = compactSpaces(n.getAttribute('alt') || '').trim();
    if (!alt && DECOR_RE.test(src)) return; // 无 alt 的装饰图（图标/logo/雪碧图）
    var label = imgLabel(n, src);
    if (src.length > IMG_SRC_MAX) src = src.slice(0, IMG_SRC_MAX) + '…';
    st.imgEmitted++;
    b.push('![' + label + '](' + src + ')');
  }

  // Go compactSpaces：仅折叠 ASCII 空白（' ','\t','\n','\r','\f','\v'）为单空格。
  // 不用 \s——JS \s 含 \u00a0 等 Unicode 空白，会与 Go 端输出产生分歧。
  function compactSpaces(s) {
    return s.replace(/[ \t\n\r\f\v]+/g, ' ');
  }

  // Go collapseBlankLines：连续 3+ 换行压成 2 个（一个空行）
  function collapseBlankLines(s) {
    while (s.indexOf('\n\n\n') >= 0) s = s.split('\n\n\n').join('\n\n');
    return s;
  }

  // 标题内文本（标题内不嵌套块级，取纯文本即可）
  function inlineText(n) {
    var sb = '';
    (function walk(c) {
      if (c.nodeType === 3) sb += compactSpaces(c.data); // TEXT_NODE
      for (var x = c.firstChild; x; x = x.nextSibling) walk(x);
    })(n);
    return sb.trim();
  }

  // 代码块文本：保留原始换行（代码语义），仅去首尾空白
  function preText(n) {
    var sb = '';
    (function walk(c) {
      if (c.nodeType === 3) sb += c.data;
      for (var x = c.firstChild; x; x = x.nextSibling) walk(x);
    })(n);
    return sb.trim();
  }

  // 读元素整数属性（缺失/非法返回默认值）
  function attrNum(n, key, def) {
    var v = parseInt(n.getAttribute(key) || '', 10);
    return isNaN(v) ? def : v;
  }

  // renderPipeTable：把 table 渲染为 Markdown 管道表格（行列关系保留）；
  // 返回 false 表示布局表格/畸形结构，调用方回退通用块级渲染。
  // 与 Go pageextract.go renderPipeTable 同构（决策 A1b 复制填充 / A2 caption
  // 独立行 / A3 首行表头 / A4 布局表格判定 / 单元格 | 移除 / 嵌套表并入 / tr 外文本忽略）。
  function renderPipeTable(tableNode, b) {
    var trs = [];
    (function walk(n) {
      if (n.nodeType !== 1) return;
      var t = n.tagName.toLowerCase();
      if (t === 'table' && n !== tableNode) return; // 嵌套 table：文本经外层单元格保留
      if (t === 'tr') { trs.push(n); return; }
      for (var c = n.firstChild; c; c = c.nextSibling) walk(c);
    })(tableNode);
    if (trs.length === 0) return false;

    function cellText(n) {
      var t = inlineText(n).split('|').join(''); // | 直接移除（转义会破坏紧凑匹配）
      return t.trim();
    }
    function hasBlockChild(n) {
      var found = false;
      (function scan(x) {
        if (found || x.nodeType !== 1) return;
        var t = x.tagName.toLowerCase();
        if (x !== n && (t === 'p' || /^h[1-6]$/.test(t))) { found = true; return; }
        for (var c = x.firstChild; c && !found; c = c.nextSibling) scan(c);
      })(n);
      return found;
    }

    // 布局表格判定：>50% 单元格内含 p/h1-h6 块级正文元素 → 回退散行。
    // 已知边界：老式 table 布局站（如 Hacker News，格子为纯文本+链接）不被此
    // 判定拦截——其内容经管道化后仍完整可检索（| 参与紧凑匹配忽略），仅形态成表。
    var blocky = 0, total = 0;
    trs.forEach(function (tr) {
      for (var c = tr.firstChild; c; c = c.nextSibling) {
        if (c.nodeType !== 1) continue;
        var ct = c.tagName.toLowerCase();
        if (ct !== 'td' && ct !== 'th') continue;
        total++;
        if (hasBlockChild(c)) blocky++;
      }
    });
    if (total > 0 && blocky * 2 > total) return false;

    // 网格填充：colspan 横向占位，rowspan 纵向复制填充
    var grid = [];
    trs.forEach(function (tr, ri) {
      var col = 0;
      for (var c = tr.firstChild; c; c = c.nextSibling) {
        if (c.nodeType !== 1) continue;
        var ct = c.tagName.toLowerCase();
        if (ct !== 'td' && ct !== 'th') continue;
        while (ri < grid.length && col < grid[ri].length && grid[ri][col] !== '') col++;
        var cs = attrNum(c, 'colspan', 1), rs = attrNum(c, 'rowspan', 1);
        if (cs < 1) cs = 1;
        if (rs < 1) rs = 1;
        var text = cellText(c);
        for (var dr = 0; dr < rs; dr++) {
          while (grid.length <= ri + dr) grid.push([]);
          for (var dc = 0; dc < cs; dc++) {
            while (grid[ri + dr].length <= col + dc) grid[ri + dr].push('');
            grid[ri + dr][col + dc] = text;
          }
        }
        col += cs;
      }
    });
    var width = 0;
    grid.forEach(function (row) { if (row.length > width) width = row.length; });
    if (width === 0) return false;

    // caption（A2）：表格前独立行
    for (var cc = tableNode.firstChild; cc; cc = cc.nextSibling) {
      if (cc.nodeType === 1 && cc.tagName.toLowerCase() === 'caption') {
        var capText = inlineText(cc).trim();
        if (capText) b.push('\n' + capText + '\n\n');
        break;
      }
    }

    // 管道行输出：首个 tr 为表头行，其后固定分隔行
    b.push('\n');
    for (var ri = 0; ri < grid.length; ri++) {
      var cells = [];
      for (var ci = 0; ci < width; ci++) cells.push(grid[ri][ci] || '');
      b.push('| ' + cells.join(' | ') + ' |\n');
      if (ri === 0) b.push('|' + new Array(width + 1).join(' --- |') + '\n');
    }
    b.push('\n');
    return true;
  }

  function findFirst(n, tag) {
    if (n.nodeType === 1 && n.tagName.toLowerCase() === tag) return n;
    for (var c = n.firstChild; c; c = c.nextSibling) {
      var r = findFirst(c, tag);
      if (r) return r;
    }
    return null;
  }

  function findTitle(doc) {
    var head = findFirst(doc, 'head');
    if (!head) return '';
    var t = findFirst(head, 'title');
    return t ? inlineText(t) : '';
  }

  // Go renderNode：深度优先渲染。块级前后换行，标题/代码块特殊格式。
  // st 为图片占位状态（extractPageText 每次 call 新建，同步渲染无重入）。
  function renderNode(n, b, st) {
    if (n.nodeType === 3) { b.push(compactSpaces(n.data)); return; } // TEXT_NODE
    if (n.nodeType !== 1) return; // 注释等
    var tag = n.tagName.toLowerCase();
    if (SKIP.has(tag)) return;
    if (tag === 'img') { emitImage(n, b, st); return; }
    if (tag === 'br') { b.push('\n'); return; }
    var h = /^h([1-6])$/.exec(tag);
    if (h) {
      b.push('\n' + new Array(+h[1] + 1).join('#') + ' ' + inlineText(n) + '\n\n');
      return;
    }
    if (tag === 'pre') { b.push('\n```\n' + preText(n) + '\n```\n\n'); return; }
    // 表格优先走管道化（行列关系保留）；布局表格/畸形结构回退通用块级渲染
    if (tag === 'table') {
      if (renderPipeTable(n, b)) return;
      b.push('\n');
      for (var tc = n.firstChild; tc; tc = tc.nextSibling) renderNode(tc, b, st);
      b.push('\n');
      return;
    }
    if (tag === 'ol') {
      // 有序列表：浏览器渲染的序号（1. 2. …）不在 DOM 文本中，渲染时补回。
      // 支持 start 属性；嵌套 ol 递归时独立计数。与 Go pageextract.go 同构。
      // li 走通用渲染（块级换行包裹），序号插到吃掉前置换行后的内容前。
      var startNum = parseInt(n.getAttribute('start') || '1', 10);
      if (isNaN(startNum)) startNum = 1;
      b.push('\n');
      var idx = startNum;
      for (var c = n.firstChild; c; c = c.nextSibling) {
        if (c.nodeType === 1 && c.tagName.toLowerCase() === 'li') {
          var lb = [];
          renderNode(c, lb, st);
          b.push(idx + '. ' + lb.join('').replace(/^\n+/, ''));
          idx++;
          continue;
        }
        renderNode(c, b, st);
      }
      b.push('\n');
      return;
    }
    if (BLOCK_SET.has(tag)) {
      b.push('\n');
      for (var c = n.firstChild; c; c = c.nextSibling) renderNode(c, b, st);
      b.push('\n');
      return;
    }
    // 行内元素：递归，不换行
    for (var c2 = n.firstChild; c2; c2 = c2.nextSibling) renderNode(c2, b, st);
  }

  // Go ExtractPageText 同构入口：HTML 源码 → 结构化文本。
  // 空输入/解析异常返回空串（调用方按无缓存处理，grab.js 回退原始 HTML）。
  function extractPageText(htmlSrc) {
    if (typeof htmlSrc !== 'string' || htmlSrc.trim().length === 0) return '';
    var doc;
    try {
      doc = new DOMParser().parseFromString(htmlSrc, 'text/html');
    } catch (e) {
      return '';
    }
    if (!doc) return '';
    var b = [];
    var st = { imgEmitted: 0, imgOmitted: 0 };
    var title = findTitle(doc);
    if (title) b.push('页面标题: ' + title + '\n\n');
    var body = findFirst(doc, 'body') || doc.documentElement;
    if (body) renderNode(body, b, st);
    // 超上限的图片统一补一行省略说明（与 Go ExtractPageText 收尾同构）
    if (st.imgOmitted > 0) b.push('\n（另省略 ' + st.imgOmitted + ' 张图）\n');
    return collapseBlankLines(b.join('')).trim();
  }

  // 统计辅助（测试/日志用）：标题行数
  function countHeadings(text) {
    return text.split('\n').filter(function (l) { return /^ {0,3}#{1,6} /.test(l); }).length;
  }

  return { extractPageText: extractPageText, countHeadings: countHeadings };
});
