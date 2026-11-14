// Absorb Visual Explanation - 渲染器
// 职责：把 parser.js 的结构化文档渲染为 .absorb-visual DOM。
// 纪律（可整体拷走复用）：不 import 任何 Absorb 模块、不读全局状态；
// Markdown 渲染经依赖注入 mdToHtml(text) → 已消毒 HTML 字符串（胶水层接 marked+DOMPurify）；
// 所有纯文本一律 textContent 落地，绝不把 LLM 原文直接 innerHTML。
// 语法契约见同目录 SPEC.md；样式命名空间见同目录 visual.css（.absorb-visual / --av-*）。
import { parseVisualDocument } from './parser.js';

// 渲染一个 visual 段（parseVisualSegments 的产物）为 DOM 节点。
// 任何异常向上抛出，由胶水层降级为普通 Markdown 渲染。
// errors（可选）：解析期错误（行号/组件/原因）会追加进该数组，由胶水层决定
// 展示或上报；渲染本身不因这些非致命问题中断（畸形部分已退化为 markdown 块）。
export function renderVisualBlock(seg, mdToHtml, errors) {
  const doc = parseVisualDocument(seg.raw, errors);
  const root = el('section', 'absorb-visual');
  root.dataset.template = doc.meta.template === 'doc' ? 'doc' : 'sheet';
  root.setAttribute('role', 'region');
  root.setAttribute('aria-label', '可视化解释' + (doc.meta.title ? '：' + doc.meta.title : ''));

  // 头部：title/subtitle 任一存在才渲染
  if (doc.meta.title || doc.meta.subtitle) {
    const head = el('header', 'av-head');
    if (doc.meta.title) {
      const h = el('h3', 'av-title');
      h.textContent = doc.meta.title;
      head.appendChild(h);
    }
    if (doc.meta.subtitle) {
      const p = el('p', 'av-subtitle');
      p.textContent = doc.meta.subtitle;
      head.appendChild(p);
    }
    root.appendChild(head);
  }

  // 导语区（第一个 ## 之前的内容）
  if (doc.preamble.blocks.length) {
    root.appendChild(buildBody('div', 'av-lead', doc.preamble.blocks, mdToHtml));
  }

  // 分节：sheet = 面板卡片；doc = 连续文档（同一 DOM 结构，布局交给 CSS）
  if (doc.sections.length) {
    const body = el('div', 'av-body');
    for (const sec of doc.sections) {
      const panel = el('section', 'av-panel');
      if (sec.title) {
        const h = el('h4', 'av-panel-title');
        h.textContent = sec.title;
        panel.appendChild(h);
      }
      panel.appendChild(buildBody('div', 'av-panel-body', sec.blocks, mdToHtml));
      body.appendChild(panel);
    }
    root.appendChild(body);
  }
  return root;
}

// ---------- 组件构建 ----------

function buildBody(tag, cls, blocks, mdToHtml) {
  const wrap = el(tag, cls);
  for (const b of blocks) wrap.appendChild(buildBlock(b, mdToHtml));
  return wrap;
}

function buildBlock(b, mdToHtml) {
  switch (b.kind) {
    case 'md':
      return mdEl('div', 'av-md', b.text, mdToHtml);
    case 'callout': {
      const box = el('aside', `av-callout av-callout-${b.variant}`);
      box.appendChild(mdEl('div', 'av-callout-body', b.text, mdToHtml));
      return box;
    }
    case 'kv': {
      // dl/dt/dd 语义；键值各一格，值经 Markdown 渲染（支持行内代码/加粗）
      const dl = el('dl', 'av-kv');
      for (const item of b.items) {
        const dt = el('dt');
        dt.textContent = item.key;
        dl.appendChild(dt);
        dl.appendChild(mdEl('dd', 'av-kv-value', item.value, mdToHtml));
      }
      return dl;
    }
    case 'timeline': {
      // ol 语义（时序/步骤有序）；节点圆点与连接线由 CSS 伪元素承担
      const ol = el('ol', 'av-timeline');
      for (const item of b.items) {
        const li = el('li');
        if (item.label) {
          const lab = el('span', 'av-tl-label');
          lab.textContent = item.label;
          li.appendChild(lab);
        }
        if (item.text) li.appendChild(mdEl('div', 'av-tl-text', item.text, mdToHtml));
        ol.appendChild(li);
      }
      return ol;
    }
    case 'tree':
      return buildTree(b.root);
    case 'chart':
      return buildChart(b);
    default:
      return mdEl('div', 'av-md', b.text || '', mdToHtml);
  }
}

function buildTree(node) {
  const ul = el('ul', 'av-tree');
  for (const child of node.children) {
    const li = el('li');
    const lab = el('span', 'av-tree-label');
    lab.textContent = child.label;
    li.appendChild(lab);
    if (child.children.length) li.appendChild(buildTree(child));
    ul.appendChild(li);
  }
  return ul;
}

// chart（bar）：纯 CSS 横向条形，条宽按最大值归一（正值下限 2% 保证可见）。
// 数据来自 parser（畸形行已被丢弃），label/数值 textContent 落地，无任何 HTML 注入面。
function buildChart(b) {
  const wrap = el('div', 'av-chart');
  wrap.setAttribute('role', 'img');
  wrap.setAttribute('aria-label', `条形图${b.title ? '：' + b.title : ''}，共 ${b.items.length} 项`);
  const head = [b.title, b.unit ? `单位：${b.unit}` : ''].filter(Boolean).join('（') +
    (b.title && b.unit ? '）' : '');
  if (head) {
    const p = el('p', 'av-chart-title');
    p.textContent = head;
    wrap.appendChild(p);
  }
  const max = Math.max(...b.items.map((it) => it.value));
  for (const it of b.items) {
    const row = el('div', 'av-chart-row');
    const lab = el('span', 'av-chart-label');
    lab.textContent = it.label;
    row.appendChild(lab);
    const track = el('span', 'av-chart-track');
    const bar = el('span', 'av-chart-bar');
    const pct = max > 0 ? (it.value / max) * 100 : 0;
    bar.style.width = (it.value > 0 && pct < 2 ? 2 : pct) + '%';
    track.appendChild(bar);
    row.appendChild(track);
    const val = el('span', 'av-chart-value');
    val.textContent = String(it.value);
    row.appendChild(val);
    wrap.appendChild(row);
  }
  return wrap;
}

// Markdown 块：内容经注入的 mdToHtml（marked + DOMPurify）消毒后落地
function mdEl(tag, cls, text, mdToHtml) {
  const node = el(tag, cls);
  node.innerHTML = mdToHtml(String(text || ''));
  return node;
}

function el(tag, cls) {
  const node = document.createElement(tag);
  if (cls) node.className = cls;
  return node;
}
