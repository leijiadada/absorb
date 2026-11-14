// Absorb Visual Explanation - 分段编排与失败态构建（胶水层的可测部分）
// 职责：把回答切分为 md / visual 段并装配节点；负责流式占位与渲染异常降级。
// 纪律（与 parser.js / renderer.js 同）：不 import 任何 Absorb 模块、不读全局状态；
// DOM 构建与 Markdown 渲染经依赖注入（deps = { el(tag, cls), mdToHtml(text), text(key) }），
// 因此可在 node --test 下用 fakeEl 桩直接单测。
// 契约见同目录 SPEC.md 设计原则 3（失败降级）与 4（流式不渲染）。
import { parseVisualSegments } from './parser.js';

// 流式占位：可视化块尚未完成时不解析，只给虚线框 + 弱化文案（panel-non-distracting）。
export function makePending(deps) {
  const { el, text } = deps;
  const box = el('div', 'absorb-visual-pending');
  box.setAttribute('aria-live', 'polite');
  box.textContent = text('visual.generating');
  return box;
}

// 降级展示：整块渲染异常时保留原始 Markdown 可读，并附一行弱化告警。
// 原因列表走 textContent 落地（不拼接 HTML），避免把异常信息变成注入面。
export function makeDegraded(raw, errors, deps) {
  const { el, mdToHtml, text } = deps;
  const box = el('div', 'absorb-visual-degraded');
  const tip = el('div', 'absorb-visual-degraded-tip');
  const reasons = (errors || [])
    .map((e) => `第${e.line || '?'}行 ${e.component}：${e.message}`)
    .join('；');
  tip.textContent = text('visual.degraded') + (reasons ? `（${reasons}）` : '');
  box.appendChild(tip);
  const body = el('div');
  body.innerHTML = mdToHtml(raw);
  box.appendChild(body);
  return box;
}

// 编排：按原文顺序返回节点数组。final=false 时 visual 段一律占位（不解析）；
// final=true 时交由注入的 renderBlock 渲染，抛异常则记录原因并降级为原始 Markdown。
// errors 由调用方持有，解析期与渲染期的问题都追加进去（非致命，不中断消息流）。
export function buildVisualContent(content, final, deps, renderBlock, errors) {
  const nodes = [];
  const mdNode = (source) => {
    const div = deps.el('div');
    div.innerHTML = deps.mdToHtml(source);
    return div;
  };
  for (const seg of parseVisualSegments(content, final ? errors : undefined)) {
    if (seg.type === 'md') {
      nodes.push(mdNode(seg.text));
      continue;
    }
    if (!final) {
      nodes.push(makePending(deps));
      continue;
    }
    try {
      nodes.push(renderBlock(seg, deps.mdToHtml, errors));
    } catch (e) {
      errors.push({ line: 0, component: 'absorb-visual', message: '渲染异常：' + ((e && e.message) || e) });
      nodes.push(makeDegraded(seg.raw, errors, deps));
    }
  }
  return nodes;
}
