// Absorb 阅读助手 - HTML 噪声剪枝库（负选择）
// 职责单一：从内容区 HTML 中剪掉不应进入 pageMd 的不可见 DOM——关闭态弹窗/tooltip
//（SPA 组件库常驻 DOM，仅切换 display）、隐藏占位、加载态、隐藏统计 iframe 等。
// 与 grab.js/pageconvert.js 解耦：输入 HTML/元素，输出剪枝后 HTML/元素，只做"负选择"，
// 不解析转换，转换器（pageconvert.js / Go ExtractPageText）的同构契约不受影响。
// 执行环境：
//   - executeScript 注入的页面隔离世界（经典脚本，挂 globalThis.__absorbPrune）
//   - vitest（jsdom，ESM/CJS 均可加载）
// 设计要点：
//   - 规则 = 数据化描述符（id + match + action）挂注册表，新噪声类型只加一条规则 + 一个 fixture
//   - 样式判定经 StyleResolver 注入（唯一运行时接缝）：浏览器传 getComputedStyle
//     （能识别 class 驱动的隐藏），测试/Node 传内联样式实现，未来 Go 端可共享同一规则表
//   - 保守剪枝：只剪 display:none / visibility:hidden / [hidden]，不碰 aria-hidden、
//     opacity:0、屏外定位（误伤可见正文的风险规则一概不用）

(function (factory) {
  var api = factory();
  if (typeof module !== 'undefined' && module.exports) {
    module.exports = api; // Node/CJS
  } else {
    (typeof globalThis !== 'undefined' ? globalThis : self).__absorbPrune = api;
  }
})(function () {
  'use strict';

  // ---------- StyleResolver（唯一运行时接缝） ----------

  // 静态 resolver：只读内联样式。任何 DOM 环境可用（jsdom/Node/浏览器），作为兜底。
  function inlineStyleResolver(el) {
    var s = el.style || {};
    return { display: s.display || '', visibility: s.visibility || '' };
  }

  // 浏览器 resolver：computed 样式可识别 class 驱动的隐藏（内联隐藏同样被覆盖，
  // 内联规则在前只是为了让统计能区分来源）。不可用（无 browsing context）时回退内联。
  function computedStyleResolver(el) {
    var view = el.ownerDocument && el.ownerDocument.defaultView;
    var cs = view && typeof view.getComputedStyle === 'function' ? view.getComputedStyle(el) : null;
    if (!cs) return inlineStyleResolver(el);
    return { display: cs.display || '', visibility: cs.visibility || '' };
  }

  // ---------- 内置规则注册表（数据化描述符） ----------

  var BUILTIN_RULES = [
    // hidden 属性（HTML 标准 + 框架通用）
    { id: 'attr-hidden', match: function (el) { return el.hasAttribute('hidden'); } },
    // 内联 display:none（Vue v-show、Element Plus 关闭态 popover、weui 弹窗等的主要形态）
    { id: 'inline-display-none', match: function (el) { return inlineStyleResolver(el).display === 'none'; } },
    { id: 'inline-visibility-hidden', match: function (el) { return inlineStyleResolver(el).visibility === 'hidden'; } },
    // computed 兜底：样式表 class 驱动的隐藏（上述内联规则未覆盖的剩余形态）
    { id: 'computed-hidden', match: function (el, ctx) {
        var s = ctx.styleOf(el);
        return s.display === 'none' || s.visibility === 'hidden';
      } }
  ];

  // initPruner 组装一个剪枝实例。rules：描述符数组；styleResolver：el → {display, visibility}。
  // 默认静态 resolver（Node 安全）；浏览器实例传 computedStyleResolver。
  function initPruner(options) {
    var rules = (options && options.rules) || BUILTIN_RULES;
    var styleOf = (options && options.styleResolver) || inlineStyleResolver;
    var ctx = { styleOf: styleOf };

    function matchFirst(el) {
      for (var i = 0; i < rules.length; i++) {
        if (rules[i].match(el, ctx)) return rules[i].id;
      }
      return null;
    }

    // 剪枝 root 的不可见后代（root 自身免疫——调用方拿到的就是内容区锚点）。
    // 先序遍历：命中即整棵子树移除且不再下探（隐藏祖先的子孙无需重复判定，
    // visibility 继承语义也由此天然覆盖）。返回 removed 统计供日志与测试断言。
    function prune(root) {
      var removed = [];
      if (!root || root.nodeType !== 1) return { root: root, removed: removed };
      (function walk(el) {
        // children 快照：遍历中会移除节点
        var kids = [];
        for (var i = 0; i < el.children.length; i++) kids.push(el.children[i]);
        for (var j = 0; j < kids.length; j++) {
          var child = kids[j];
          var hit = matchFirst(child);
          if (hit) {
            removed.push({
              rule: hit,
              tag: child.tagName,
              textLen: (child.textContent || '').length
            });
            if (child.parentNode) child.parentNode.removeChild(child);
          } else {
            walk(child);
          }
        }
      })(root);
      return { root: root, removed: removed };
    }

    return { prune: prune, rules: rules };
  }

  // ---------- 浏览器默认实例与字符串便捷入口 ----------

  var defaultPruner = initPruner({ styleResolver: computedStyleResolver });

  // 字符串便捷形态：HTML → 栽入 doc 的离屏容器 → 剪枝 → 序列化。
  // 离屏 live 挂载是关键：只有进入文档，class 驱动的隐藏才能被 getComputedStyle 识别；
  // 内联样式随 DOM 保留。host 不设 display:none（子元素各自 computed 不受影响），
  // 仅移出视口避免任何视觉闪现。innerHTML 赋值不执行脚本（HTML5 语义），安全。
  // 失败向上抛出，由调用方决定回退策略（grab.js 回退原始 HTML）。
  function pruneHtml(html, doc) {
    if (!html) return { html: html || '', removed: [] };
    var host = doc.createElement('div');
    host.setAttribute('style',
      'position:absolute;left:-9999px;top:0;width:0;height:0;overflow:hidden;');
    host.innerHTML = html;
    (doc.body || doc.documentElement).appendChild(host);
    try {
      var r = defaultPruner.prune(host);
      return { html: host.innerHTML, removed: r.removed };
    } finally {
      if (host.parentNode) host.parentNode.removeChild(host);
    }
  }

  return {
    BUILTIN_RULES: BUILTIN_RULES,
    initPruner: initPruner,
    inlineStyleResolver: inlineStyleResolver,
    computedStyleResolver: computedStyleResolver,
    pruneRoot: function (root) { return defaultPruner.prune(root); },
    pruneHtml: pruneHtml
  };
});
