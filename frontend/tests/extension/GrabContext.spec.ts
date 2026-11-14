// grab.js 划词采集逻辑测试。grab.js 是 executeScript 注入的经典脚本（无 ESM 导出），
// 与 pageconvert.spec.ts 同法：读源码后在 jsdom window 中 eval，函数声明挂到 window 上调用。
// 覆盖：isComplete 判定、windowSlice 窗口裁剪、collectSelectionLinks 链接收集、
// grabPageHtml 三级降级链（Defuddle 缺失形态）、convertPageHtml 转换/截断/剪枝接缝。
import { describe, it, expect } from 'vitest'
import { readFileSync } from 'node:fs'
import { join, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'
import { JSDOM } from 'jsdom'

const here = dirname(fileURLToPath(import.meta.url))
const GRAB_SRC = readFileSync(join(here, '../../../extension/grab.js'), 'utf8')

// 每个用例独立 JSDOM 环境。预置空转换器桩：避免 eval 尾部 grabPageOnly() 的
// "pageconvert 未注入"告警；需要真转换行为的用例按需覆盖 __absorbPageConvert。
function loadGrab(bodyHtml: string, url = 'https://fixture.local/page') {
  // runScripts: 'outside-only' 使 window.eval 生效（经典脚本注入语义，同 pageconvert.spec.ts）
  const dom = new JSDOM(`<!DOCTYPE html><html><body>${bodyHtml}</body></html>`, {
    url,
    runScripts: 'outside-only',
  })
  dom.window.__absorbPageConvert = { extractPageText: () => '' }
  dom.window.eval(GRAB_SRC)
  return dom
}

describe('grab.js — isComplete 完整性判定', () => {
  const cases: [string, string, boolean][] = [
    ['长文本直接视为段落', '字'.repeat(101), true],
    ['句末中文句号视为完整', '这是一个完整的句子。', true],
    ['句末问号视为完整', '这个功能怎么用?', true],
    ['含换行且有句读视为完整', '第一行。\n第二行。', true],
    ['短片段无句读不完整', '不完整片段', false],
  ]
  for (const [name, input, want] of cases) {
    it(name, () => {
      const dom = loadGrab('')
      expect((dom.window as any).isComplete(input)).toBe(want)
    })
  }
})

describe('grab.js — windowSlice 段落窗口裁剪', () => {
  it('选区不在全文中 → 直接截取前 1200 字', () => {
    const dom = loadGrab('')
    const full = '短全文'
    expect((dom.window as any).windowSlice(full, '不存在的选区', 300, 300)).toBe(full)
  })

  it('选区居中 → 前后窗口 + 双侧省略号', () => {
    const dom = loadGrab('')
    const full = 'a'.repeat(400) + 'TARGET' + 'b'.repeat(400)
    const out = (dom.window as any).windowSlice(full, 'TARGET', 300, 300) as string
    expect(out.startsWith('…')).toBe(true)
    expect(out.endsWith('…')).toBe(true)
    expect(out).toContain('TARGET')
    expect(out).toHaveLength(1 + 300 + 6 + 300 + 1)
  })

  it('选区在开头 → 无前置省略号', () => {
    const dom = loadGrab('')
    const full = 'TARGET' + 'a'.repeat(1000)
    const out = (dom.window as any).windowSlice(full, 'TARGET', 300, 300) as string
    expect(out.startsWith('TARGET')).toBe(true)
    expect(out.endsWith('…')).toBe(true)
  })
})

describe('grab.js — collectSelectionLinks 选区链接收集', () => {
  it('收集 label+href、相对 URL 解析、同 href 去重取首个 label、空 label 过滤', () => {
    const dom = loadGrab('')
    const doc = dom.window.document
    const frag = doc.createDocumentFragment()
    const mk = (href: string, label: string) => {
      const a = doc.createElement('a')
      a.setAttribute('href', href)
      a.textContent = label
      return a
    }
    frag.append(
      mk('https://ext.example/x', '外部链接'),
      mk('rel/page.html', '相对链接'),
      mk('https://ext.example/x', '重复链接'), // 同 href 去重：保留首个 label
      mk('https://no-label.example', ''),       // 空 label 过滤
    )
    const fakeSel = { rangeCount: 1, getRangeAt: () => ({ cloneContents: () => frag }) }
    const out = (dom.window as any).collectSelectionLinks(fakeSel) as string
    expect(out).toBe(
      '划选内容引用链接：\n' +
      '1. [外部链接](https://ext.example/x)\n' +
      '2. [相对链接](https://fixture.local/rel/page.html)')
  })

  it('无选区范围返回空串', () => {
    const dom = loadGrab('')
    expect((dom.window as any).collectSelectionLinks({ rangeCount: 0 })).toBe('')
  })
})

describe('grab.js — grabPageHtml 三级降级链', () => {
  it('选择器命中：#readme 优先于 main（按选择器表顺序）', () => {
    const fill = 'x'.repeat(300)
    const dom = loadGrab(`<div id="readme">${fill}</div><main>${fill}</main>`)
    const out = (dom.window as any).grabPageHtml(null) as string
    expect(out.startsWith('<div id="readme">')).toBe(true)
  })

  it('选择器全不中 → 选区所在大容器兜底（>2000 字符块级）', () => {
    const dom = loadGrab('<div id="wrap"><p id="p">锚点</p>' + 'x'.repeat(2500) + '</div>')
    const anchorNode = dom.window.document.getElementById('p')!.firstChild
    const out = (dom.window as any).grabPageHtml({ anchorNode }) as string
    expect(out.startsWith('<div id="wrap">')).toBe(true)
  })

  it('全部失败 → 空串（宁缺勿错，无页面缓存信号）', () => {
    const dom = loadGrab('<div>太短</div>')
    expect((dom.window as any).grabPageHtml(null)).toBe('')
  })
})

describe('grab.js — convertPageHtml / grabPageOnly 转换管线', () => {
  const fill = 'x'.repeat(300)

  it('转换器产出超 100K 截断到 MAX_PAGE_TEXT_CHARS', () => {
    const dom = loadGrab(`<main>${fill}</main>`)
    dom.window.__absorbPageConvert = { extractPageText: () => 'y'.repeat(100500) }
    const r = (dom.window as any).grabPageOnly() as { pageMd: string }
    expect(r.pageMd).toHaveLength(100000)
  })

  it('转换器缺失 → pageMd 为空串（本次无页面缓存）', () => {
    const dom = loadGrab(`<main>${fill}</main>`)
    delete (dom.window as any).__absorbPageConvert
    const r = (dom.window as any).grabPageOnly() as { pageMd: string; url: string }
    expect(r.pageMd).toBe('')
    expect(r.url).toBe('https://fixture.local/page')
  })

  it('剪枝接缝：convertPageHtml 输入先经 __absorbPrune 再进转换器', () => {
    const dom = loadGrab(`<main>${fill}</main>`)
    ;(dom.window as any).__absorbPrune = {
      pruneHtml: (h: string) => ({ html: h + 'PRUNED|', removed: [] }),
    }
    dom.window.__absorbPageConvert = { extractPageText: (h: string) => 'MD[' + h + ']' }
    const r = (dom.window as any).grabPageOnly() as { pageMd: string }
    expect(r.pageMd).toContain('PRUNED|')
    expect(r.pageMd.startsWith('MD[')).toBe(true)
  })

  it('剪枝抛异常 → 回退原始 HTML，转换不受阻（宁缺勿错不阻断）', () => {
    const dom = loadGrab(`<main>${fill}</main>`)
    ;(dom.window as any).__absorbPrune = {
      pruneHtml: () => { throw new Error('prune boom') },
    }
    dom.window.__absorbPageConvert = { extractPageText: (h: string) => 'MD[' + h.length + ']' }
    const r = (dom.window as any).grabPageOnly() as { pageMd: string }
    expect(r.pageMd).not.toContain('PRUNED')
    expect(r.pageMd).toContain('MD[')
  })
})

describe('grab.js — 重复注入回归（隔离世界跨 executeScript 调用持久）', () => {
  // 真实场景：quote-collector 划词采集与 conversation.js 每次提问都会把同一份
  // grab.js 注入同一隔离世界。顶层 const/let 二次声明会 SyntaxError 使整份脚本
  // 求值失败（results[0].result = undefined，pageMd 静默丢失）。回归：同一窗口
  // 连续求值两次（loadGrab 内已完成第一次）必须不抛错，且第二次仍产出有效结果。
  it('连续注入两次：无 SyntaxError，第二次 grabPageOnly 仍返回有效结果', () => {
    const fill = 'x'.repeat(300)
    const dom = loadGrab(`<main>${fill}</main>`)
    dom.window.__absorbPageConvert = { extractPageText: () => 'MD2' }
    // 第二次注入：同一隔离世界再次求值整份脚本
    expect(() => (dom.window as any).eval(GRAB_SRC)).not.toThrow()
    const r = (dom.window as any).grabPageOnly() as { url: string; pageMd: string }
    expect(r.url).toBe('https://fixture.local/page')
    expect(r.pageMd).toBe('MD2')
  })
})
