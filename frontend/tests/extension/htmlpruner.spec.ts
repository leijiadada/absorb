// html-pruner 单元与集成回归。被测对象 extension/html-pruner.js（UMD，createRequire 加载）。
// fixture 覆盖三类隐藏形态（内联 / [hidden] 属性 / class 驱动的 computed 隐藏——
// jsdom 的 getComputedStyle 支持 <style> 级联，见 vitest 环境）；
// 集成断言用 docs/page-corpus/spa-modal-noise（真实 SPA 关闭态弹窗噪音页，缺失时跳过）。
import { describe, it, expect } from 'vitest'
import { readFileSync, statSync } from 'node:fs'
import { join, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'
import { createRequire } from 'node:module'
import { JSDOM } from 'jsdom'

const require = createRequire(import.meta.url)
const here = dirname(fileURLToPath(import.meta.url))
// tests/extension/ → frontend/tests → frontend → absorb-agent
const pruner = require('../../../extension/html-pruner.js') as {
  initPruner: (opts?: {
    rules?: { id: string; match: (el: Element, ctx: { styleOf: (el: Element) => { display: string; visibility: string } }) => boolean }[]
    styleResolver?: (el: Element) => { display: string; visibility: string }
  }) => { prune: (root: Element) => { root: Element; removed: { rule: string; tag: string; textLen: number }[] } }
  pruneHtml: (html: string, doc: Document) => { html: string; removed: { rule: string; tag: string; textLen: number }[] }
  computedStyleResolver: (el: Element) => { display: string; visibility: string }
  BUILTIN_RULES: { id: string }[]
}
const pc = require('../../../extension/pageconvert.js') as { extractPageText: (h: string) => string }

// 带样式表的文档环境（computed 级联需要）
function docWith(bodyHtml: string, headStyle = ''): { dom: JSDOM; doc: Document } {
  const dom = new JSDOM(`<!DOCTYPE html><html><head><style>${headStyle}</style></head><body>${bodyHtml}</body></html>`)
  return { dom, doc: dom.window.document }
}

describe('html-pruner — 规则 fixture', () => {
  it('内联 display:none 子树整棵剪除，可见内容保留，removed 统计正确', () => {
    const { doc } = docWith('<p>正文段落</p><div style="display: none"><p>弹窗噪声</p><span>嵌套噪声</span></div><p>尾段</p>')
    const r = pruner.pruneHtml(doc.body.innerHTML, doc)
    expect(r.removed).toHaveLength(1)
    expect(r.removed[0]).toMatchObject({ rule: 'inline-display-none', tag: 'DIV' })
    expect(r.removed[0].textLen).toBeGreaterThan(0)
    const out = pc.extractPageText(r.html)
    expect(out).toContain('正文段落')
    expect(out).toContain('尾段')
    expect(out).not.toContain('弹窗噪声')
    expect(out).not.toContain('嵌套噪声')
  })

  it('内联 visibility:hidden 剪除', () => {
    const { doc } = docWith('<p>可见</p><div style="visibility: hidden"><p>隐藏噪声</p></div>')
    const r = pruner.pruneHtml(doc.body.innerHTML, doc)
    expect(r.removed.map((x) => x.rule)).toEqual(['inline-visibility-hidden'])
    expect(pc.extractPageText(r.html)).not.toContain('隐藏噪声')
  })

  it('hidden 属性剪除', () => {
    const { doc } = docWith('<p>可见</p><div hidden><p>attr 噪声</p></div>')
    const r = pruner.pruneHtml(doc.body.innerHTML, doc)
    expect(r.removed.map((x) => x.rule)).toEqual(['attr-hidden'])
    expect(pc.extractPageText(r.html)).not.toContain('attr 噪声')
  })

  it('class 驱动隐藏（computed 级联）剪除——内联规则覆盖不到的形态', () => {
    const { doc } = docWith('<p>可见</p><div class="popper-noise"><p>class 噪声</p></div>', '.popper-noise { display: none }')
    const r = pruner.pruneHtml(doc.body.innerHTML, doc)
    expect(r.removed.map((x) => x.rule)).toEqual(['computed-hidden'])
    expect(pc.extractPageText(r.html)).not.toContain('class 噪声')
  })

  it('命中即整棵子树剪除且不再下探（隐藏容器内的隐藏子块不重复计数）', () => {
    const { doc } = docWith(
      '<div style="display:none"><span>外层噪声</span><div style="display:none"><span>内层噪声</span></div></div>')
    const r = pruner.pruneHtml(doc.body.innerHTML, doc)
    expect(r.removed).toHaveLength(1)
    expect(pc.extractPageText(r.html)).not.toContain('内层噪声')
  })

  it('root 自身隐藏不被剪（调用方拿到的就是内容区锚点）', () => {
    const dom = new JSDOM('<!DOCTYPE html><html><body><div id="root" style="display:none"><p>锚点内容</p></div></body></html>')
    const root = dom.window.document.getElementById('root')!
    const r = pruner.pruneRoot(root)
    expect(r.removed).toHaveLength(0)
    expect(root.textContent).toContain('锚点内容')
  })

  it('无隐藏内容时原样返回（结构零改动）', () => {
    // fixture 自带 tbody：HTML 解析会规范化 <table><tr> → <table><tbody><tr>，与剪枝无关
    const html = '<div class="a"><p>段落一</p><table><tbody><tr><td>表格</td></tr></tbody></table></div>'
    const { doc } = docWith(html)
    const r = pruner.pruneHtml(html, doc)
    expect(r.removed).toHaveLength(0)
    expect(r.html).toBe(html)
  })

  it('空输入原样返回', () => {
    const { doc } = docWith('')
    expect(pruner.pruneHtml('', doc)).toEqual({ html: '', removed: [] })
  })

  it('内置规则表为数据化描述符（可扩展性的契约保障）', () => {
    expect(pruner.BUILTIN_RULES.map((r) => r.id)).toEqual([
      'attr-hidden', 'inline-display-none', 'inline-visibility-hidden', 'computed-hidden',
    ])
    // 自定义规则可注入并生效
    const { doc } = docWith('<p>可见</p><div data-track="ad"><p>自定义噪声</p></div>')
    const custom = pruner.initPruner({
      rules: [{ id: 'ad-marker', match: (el) => el.getAttribute('data-track') === 'ad' }],
    })
    const host = doc.createElement('div')
    host.innerHTML = doc.body.innerHTML
    doc.body.appendChild(host)
    const r = custom.prune(host)
    expect(r.removed.map((x) => x.rule)).toEqual(['ad-marker'])
    expect(pc.extractPageText(host.innerHTML)).not.toContain('自定义噪声')
  })
})

describe('html-pruner — spa-modal-noise 集成断言（真实噪音页）', () => {
  const exists = (p: string) => { try { statSync(p); return true } catch { return false } }
  const srcPath = join(here, '../../../docs/page-corpus/spa-modal-noise/source.html')
  const hasCorpus = exists(srcPath)

  const INTEGRATION_TIMEOUT = 60_000

  it.skipIf(!hasCorpus)('关闭态弹窗噪音归零，壳内可见内容保留，产出回落到千字级', () => {
    const html = readFileSync(srcPath, 'utf8')
    // 剪枝前：弹窗正文与序列号大量存在（通知弹窗×12 常驻 DOM）
    const raw = pc.extractPageText(html)
    expect((raw.match(/尊敬的用户/g) || []).length).toBeGreaterThanOrEqual(12)
    expect((raw.match(/SA1081/g) || []).length).toBeGreaterThanOrEqual(100)

    const r = pruner.pruneHtml(html, new JSDOM('').window.document)
    // 108 个隐藏块 / 5 万+ textContent 字符被剪（数量断言放宽到量级，防站点无关抖动）
    expect(r.removed.length).toBeGreaterThan(80)
    expect(r.removed.reduce((s, x) => s + x.textLen, 0)).toBeGreaterThan(40000)

    // 剪枝后：弹窗噪音归零，站点壳文本保留，转换产出从 3 万触顶回落到千字级
    const pruned = pc.extractPageText(r.html)
    expect(pruned).not.toContain('尊敬的用户')
    expect((pruned.match(/SA1081/g) || []).length).toBe(0)
    expect(pruned).toContain('数据流动治理平台')
    expect([...pruned].length).toBeLessThan(5000)
  }, INTEGRATION_TIMEOUT)
})
