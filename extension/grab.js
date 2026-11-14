// Absorb 阅读助手 - 划词上下文采集脚本
// 以 chrome.scripting.executeScript({files:[...]}) 注入页面隔离世界执行，
// 依赖同批注入的 vendor/defuddle.min.js（Defuddle 主内容提取）、html-pruner.js
// （噪声剪枝，负选择）与 pageconvert.js（HTML → 结构化文本）提供的全局。
// 文件末尾以 grabContextInPage() 表达式收尾，返回值经 results[0].result 回传。

// 结构化文本（pageMd）上限：Go 端 pageTextMaxLen(30000) 截断仍兜底，100K 留足余量。
// 顶层必须用 var（而非 const/let）：本文件会被 executeScript 重复注入同一隔离世界
// （quote-collector 划词采集与 conversation.js 每次提问各注入一次），
// const/let 二次声明直接 SyntaxError 使整份脚本求值失败；var 重复声明 = 重新赋值。
var MAX_PAGE_TEXT_CHARS = 100000;

// 敏感日志开关：选中句/段落日志含页面文本内容，发布版保持静默，排查时临时打开
var DEBUG = false;

// 收集选区内 <a> 链接：保留"显示文案+真实URL"对应关系内联进参考材料，
// 供 LLM 结合上下文判断用 fetch_page 打开哪个链接。保持 text 纯文本做内部逻辑不变。
function collectSelectionLinks(s) {
  const map = new Map();
  for (let i = 0; i < s.rangeCount; i++) {
    const frag = s.getRangeAt(i).cloneContents();
    frag.querySelectorAll('a[href]').forEach((a) => {
      const label = (a.textContent || '').trim();
      const raw = (a.getAttribute('href') || '').trim();
      if (!label || !raw) return;
      const href = new URL(raw, location.href).href;
      if (!map.has(href)) map.set(href, label);
    });
  }
  if (map.size === 0) return '';
  const lines = ['划选内容引用链接：'];
  let i = 1;
  for (const [href, label] of map) lines.push(i++ + '. [' + label + '](' + href + ')');
  return lines.join('\n');
}

// 页面级上下文：标题 + 标题路径（H1→H3 链）+ meta description
// 用于锚定选中术语所属领域/文档位置，消除"Runner"这类通用词的歧义
function grabPageContext(anchorEl) {
  const parts = [];
  const title = (document.title || '').trim().slice(0, 200);
  if (title) parts.push('页面标题: ' + title);

  // 从选区锚点向上 + 向前回溯，收集最近的 H1/H2/H3，组成"文档路径"
  const headings = [];
  if (anchorEl) {
    let cur = anchorEl;
    while (cur && headings.length < 3) {
      // 当前元素或其前面的兄弟中最近的标题
      let sib = cur;
      while (sib) {
        if (/^H[1-3]$/.test(sib.tagName || '')) {
          const t = (sib.textContent || '').trim();
          if (t && headings.indexOf(t) < 0) headings.unshift(t.slice(0, 80));
          break;
        }
        sib = sib.previousElementSibling;
      }
      cur = cur.parentElement;
    }
  }
  if (headings.length) parts.push('所在章节: ' + headings.join(' > '));

  const metaDesc = document.querySelector('meta[name="description"]');
  if (metaDesc) {
    const d = (metaDesc.getAttribute('content') || '').trim().slice(0, 200);
    if (d) parts.push('页面描述: ' + d);
  }
  return parts.join('\n');
}

// 逐级降级定位主内容区 HTML（pageMd 的输入源，转换见 pageconvert.js）：
//   0. Defuddle 主内容提取（噪音过滤，评分算法 + 站点专用规则）
//   1. 常见选择器（已知站点结构的精确回退）
//   2. 选区所在大块级容器（划词语义锚定；BODY 含内——极简页面正文直挂 body）
// 全部失败返回空串 → pageMd 为空 → 本次划词无页面缓存（宁缺勿错：
// expand_context 返回无缓存信号，主问答不受影响；此时正文已完整存在于划词 text/paragraph）。
// 避免全页 outerHTML 过大（如 GitHub 600K+）。
// sel：当前选区（供第 2 级容器回溯定位用）
function grabPageHtml(sel) {
  // 0. Defuddle 主内容提取（评分算法 + 站点专用规则，对陌生站点泛化最优）。
  //    只做"提质不兜底"：解析异常或产出过薄时静默落回下方既有链路。
  try {
    if (typeof Defuddle === 'function') {
      const r = new Defuddle(document).parse();
      const bodyLen = ((document.body && document.body.textContent) || '').trim().length;
      // 健康检查：>500 字符拦截"只抓到标题"；>正文全文 5% 拦截"导航壳页"
      if (r && r.content && r.content.length > 500 && (!bodyLen || r.content.length > bodyLen * 0.05)) {
        console.log('[Absorb] 内容区来源: Defuddle,', r.content.length, '字符');
        return r.content;
      }
      console.log('[Absorb] Defuddle 未采纳(content:',
        r && r.content ? r.content.length + '字符' : '空', '/ body 全文:', bodyLen, '字符)，回退选择器链路');
    }
  } catch (e) {
    console.warn('[Absorb] Defuddle 提取失败，回退选择器链路:', e);
  }

  // 1. 常见选择器匹配主内容区
  const selectors = [
    '#readme',                        // GitHub README
    '#repo-content-pjax-container',   // GitHub 仓库主内容
    '.markdown-body',                 // GitHub markdown 渲染
    '#mw-content-text',               // Wikipedia
    '[role="main"]',                  // ARIA 语义
    'main',                           // HTML5 语义
    'article',                        // HTML5 语义
    '#content', '.content',           // 常见 id/class
    '.post-content', '.entry-content', // 博客
    '#docs', '.documentation',        // 文档站
  ];
  for (const s of selectors) {
    const el = document.querySelector(s);
    if (el && el.textContent.trim().length > 200) {
      console.log('[Absorb] 内容区来源: 选择器', s, ',', el.outerHTML.length, '字符');
      return el.outerHTML;
    }
  }

  // 2. 选区所在大容器（选区向上找到第一个 >2000 字符的块级容器；BODY 含内——
  //    极简页面正文直挂 body 时即命中，不再需要独立兜底层）
  if (sel && sel.anchorNode) {
    let el = sel.anchorNode.nodeType === 1 ? sel.anchorNode : sel.anchorNode.parentElement;
    while (el) {
      const textLen = (el.textContent || '').trim().length;
      if (textLen > 2000 && /^(DIV|ARTICLE|SECTION|MAIN|TD|BLOCKQUOTE|LI|BODY)$/i.test(el.tagName)) {
        console.log('[Absorb] 内容区来源: 选区容器', el.tagName, ',', el.outerHTML.length, '字符');
        return el.outerHTML;
      }
      el = el.parentElement;
    }
  } else if (document.body && (document.body.textContent || '').trim().length > 2000) {
    // 2'. 无选区形态（无划词提问的页面采集）：正文直挂 body 的极简页面直接取 body
    console.log('[Absorb] 内容区来源: body（无选区极简页）,', document.body.innerHTML.length, '字符');
    return document.body.innerHTML;
  }

  // 全部失败：返回空串（宁缺勿错，见函数头注释）
  return '';
}

// 噪声剪枝（负选择，规则与接缝见 html-pruner.js）：转换前剔除关闭态弹窗/隐藏元素等
// 不应进入 pageMd 的 DOM（SPA 通知弹窗常驻 DOM 且 display:none，实测曾占满 30K 预算）。
// 库缺失或异常时回退原始 HTML——只影响噪音过滤，不阻断采集主链路（宁缺勿错）。
function pruneNoise(rawHtml) {
  const P = (typeof globalThis.__absorbPrune === 'object' && globalThis.__absorbPrune) || null;
  if (!P || typeof P.pruneHtml !== 'function') return rawHtml;
  try {
    const r = P.pruneHtml(rawHtml, document);
    if (r.removed.length) {
      const chars = r.removed.reduce((s, x) => s + x.textLen, 0);
      console.log('[Absorb] 剪枝移除', r.removed.length, '个隐藏块,', chars, '字符');
    }
    return r.html;
  } catch (e) {
    console.warn('[Absorb] 剪枝失败，使用原始 HTML:', e);
    return rawHtml;
  }
}

// 整页 HTML → 结构化文本（pageMd），grabContextInPage 与 grabPageOnly 共用。
// 转换器缺失/异常/产出为空均返回空串（本次无页面缓存，宁缺勿错）。
function convertPageHtml(rawHtml) {
  if (!rawHtml) return '';
  rawHtml = pruneNoise(rawHtml);
  if (typeof globalThis.__absorbPageConvert === 'object' && globalThis.__absorbPageConvert) {
    try {
      let md = globalThis.__absorbPageConvert.extractPageText(rawHtml) || '';
      if (md.length > MAX_PAGE_TEXT_CHARS) {
        console.log('[Absorb] pageMd 超限截断:', md.length, '→', MAX_PAGE_TEXT_CHARS);
        md = md.slice(0, MAX_PAGE_TEXT_CHARS);
      }
      return md;
    } catch (e) {
      console.warn('[Absorb] 页面转换失败，本次无页面缓存:', e);
      return '';
    }
  }
  console.warn('[Absorb] pageconvert 未注入，本次无页面缓存');
  return '';
}

// 无划词形态的页面采集（sidepanel 无划词提问时注入执行）：
// 仅取整页结构化文本，无选区/段落上下文。grabPageHtml(null) 走 Defuddle/选择器/body 链。
function grabPageOnly() {
  return { url: location.href, pageMd: convertPageHtml(grabPageHtml(null)) };
}

// 判定划词是否已是完整语句/段落
function isComplete(t) {
  if (t.length > 100) return true; // 长度够通常已是段落
  if (/[。！？!?…]$/.test(t) && (t.match(/[。！？!?]/g) || []).length >= 1) return true;
  if (/\n/.test(t) && /[。！？!?]/.test(t)) return true;
  return false;
}

// 从大段落里取选中句前后 N 字
function windowSlice(full, selected, before, after) {
  const idx = full.indexOf(selected);
  if (idx < 0) return full.slice(0, 1200);
  const start = Math.max(0, idx - before);
  const end = Math.min(full.length, idx + selected.length + after);
  return (start > 0 ? '…' : '') + full.slice(start, end) + (end < full.length ? '…' : '');
}

function grabContextInPage() {
  const sel = window.getSelection();
  if (!sel || sel.isCollapsed) return null;
  const text = sel.toString().trim();
  if (!text) return null;
  const url = location.href;

  const linksBlock = collectSelectionLinks(sel);
  // finalText：对外展示的参考材料 = 纯文本 + 内联链接块（有链接才追加）
  const finalText = linksBlock ? text + '\n\n' + linksBlock : text;

  const pageContext = grabPageContext(sel.anchorNode && sel.anchorNode.nodeType === 1
    ? sel.anchorNode : (sel.anchorNode && sel.anchorNode.parentElement));

  // 整页内容 → 结构化文本（pageMd）：扩展端直接转换（与 Go 端 ExtractPageText 同构，
  // 见 pageconvert.js），Go 端免转换直读入页面缓存。
  // 转换不可用或产出为空 → pageMd 为空，本次划词无页面缓存（宁缺勿错：
  // expand_context 返回无缓存信号，主问答不受影响）；grabPageHtml 仍三级降级取最优内容区。
  const pageMd = convertPageHtml(grabPageHtml(sel));

  const complete = isComplete(text);
  if (DEBUG) console.log('[Absorb] 选中句:', JSON.stringify(text), '长度', text.length, 'isComplete =', complete);
  if (complete) {
    console.log('[Absorb] 判定完整，不抓段落');
    return { text: finalText, paragraph: '', pageContext, url, pageMd, isComplete: true };
  }

  // 找选区所在的块级元素：每轮必向上走，避免死循环
  let el = sel.anchorNode;
  if (el && el.nodeType !== 1) el = el.parentElement;
  let depth = 0;
  while (el) {
    depth++;
    if (/^(P|DIV|LI|TD|ARTICLE|SECTION|BLOCKQUOTE|PRE|H[1-6])$/i.test(el.tagName)) {
      const full = (el.textContent || '').trim();
      if (DEBUG) console.log('[Absorb] 层' + depth, el.tagName, '全文:', full.slice(0, 300), '| 长度', full.length, '等于选中句?', full === text);
      if (full && full !== text) {
        const para = full.length < 1200 ? full : windowSlice(full, text, 300, 300);
        if (DEBUG) console.log('[Absorb] 采用作段落上下文，para:', para);
        return { text: finalText, paragraph: para, pageContext, url, pageMd, isComplete: false };
      }
    }
    el = el.parentElement; // 必向上，最终 el=null 退出
  }
  console.log('[Absorb] 向上遍历', depth, '层未找到更大块级元素，paragraph 为空');
  return { text: finalText, paragraph: '', pageContext, url, pageMd, isComplete: true };
}

// 注入即执行（executeScript 的 result）：
// 有划词 → 完整上下文 { text, paragraph, pageContext, url, pageMd, isComplete }（quote-collector 消费）；
// 无划词 → { url, pageMd }（sidepanel 无划词提问时采集当前页，conversation.js 消费）
grabContextInPage() || grabPageOnly();
