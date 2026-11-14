// pageconvert.spec.ts — 划词整页 HTML → 结构化文本（pageMd）转换器测试
// 被测对象 extension/pageconvert.js（UMD，createRequire 加载走 module.exports 分支）。
// golden 快照由 pageconvert.js + 真实 vendor/defuddle.min.js 对 docs/defuddle-coverage
// 五个页面快照生成（快照同时兼任 defuddle 升级回归基线，见 docs/defuddle-coverage/README.md）。
import { describe, it, expect } from 'vitest'
import { readFileSync, statSync, readdirSync } from 'node:fs'
import { join, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'
import { createRequire } from 'node:module'
import { JSDOM } from 'jsdom'

const require = createRequire(import.meta.url)
// tests/extension/ → frontend/tests → frontend → absorb-agent（被测对象与语料都在 frontend 之外）
const here = dirname(fileURLToPath(import.meta.url))
const pc = require('../../../extension/pageconvert.js') as {
  extractPageText: (html: string) => string
  countHeadings: (text: string) => number
}
const DEFDUDDLE_SRC = readFileSync(join(here, '../../../extension/sidepanel/vendor/defuddle.min.js'), 'utf8')
const FIXTURES = join(here, '../../../docs/defuddle-coverage')
const SNAPSHOTS = join(here, './fixtures/page-md')

// 本地资产探测：docs/ 不入库，缺失时相关用例整组跳过（避免新环境/CI 误报红灯）
const exists = (p: string) => { try { statSync(p); return true } catch { return false } }
const HAS_GOLDEN_FIXTURES = exists(FIXTURES)

// 复现 grab.js 第 0 级提取：jsdom + vendor defuddle → 主内容 HTML
function defuddleContent(html: string): string {
  const dom = new JSDOM(html, { runScripts: 'outside-only', url: 'https://fixture.local/page' })
  dom.window.eval(DEFDUDDLE_SRC)
  const r = dom.window.eval('new Defuddle(document).parse()') as { content?: string } | null
  return (r && r.content) || ''
}

const GOLDEN = ['github', 'wikipedia', 'mdn', 'blog', 'hackernews']

describe('pageconvert.extractPageText — golden 回归（含 defuddle 升级基线）', () => {
  // 大 fixture（wikipedia 305KB）的 jsdom 解析 + Defuddle 遍历超过默认 5s，放宽到 30s
  const GOLDEN_TIMEOUT = 30_000
  for (const name of GOLDEN) {
    // fixture 在 docs/（不入库），缺失时跳过——与语料回归、html-pruner 集成断言同口径
    it.skipIf(!HAS_GOLDEN_FIXTURES)(`${name}：与固化快照逐字符一致`, () => {
      const html = readFileSync(join(FIXTURES, `${name}.html`), 'utf8')
      // 快照入库为 LF；Windows core.autocrlf=true 检出后成 CRLF，会让逐字符比较全红
      const want = readFileSync(join(SNAPSHOTS, `${name}.txt`), 'utf8').replace(/\r\n/g, '\n')
      expect(pc.extractPageText(defuddleContent(html))).toBe(want)
    }, GOLDEN_TIMEOUT)
  }
})

describe('pageconvert.extractPageText — 结构约定', () => {
  it('标题层级映射 h1-h6 → #×level', () => {
    const out = pc.extractPageText('<html><body><h1>一级</h1><p>x</p><h3>三级</h3><p>y</p><h6>六级</h6></body></html>')
    expect(out).toContain('# 一级')
    expect(out).toContain('### 三级')
    expect(out).toContain('###### 六级')
    expect(pc.countHeadings(out)).toBe(3)
  })

  it('pre 代码块转围栏且保留原始换行', () => {
    const out = pc.extractPageText('<body><pre>line1\n  line2</pre><p>after</p></body>')
    expect(out).toContain('```\nline1\n  line2\n```')
    expect(out).toContain('after')
  })

  it('script/style 内容不泄漏，可见文本保留', () => {
    const out = pc.extractPageText('<body><style>.x{color:red}</style><script>var secret=1</script><p>可见内容</p></body>')
    expect(out).toBe('可见内容')
  })

  it('空输入/空白输入返回空串', () => {
    expect(pc.extractPageText('')).toBe('')
    expect(pc.extractPageText('   \n\t ')).toBe('')
  })

  it('连续空行压一、首尾 trim（与 Go collapseBlankLines 同语义）', () => {
    const out = pc.extractPageText('<body><p>a</p><div><br><br><br></div><p>b</p></body>')
    expect(out).not.toContain('\n\n\n')
    expect(out.startsWith('a')).toBe(true)
    expect(out.endsWith('b')).toBe(true)
  })

  it('header 内 title 产出首行约定（body 级兜底路径）', () => {
    const out = pc.extractPageText('<html><head><title>页面T</title></head><body><p>x</p></body></html>')
    expect(out.startsWith('页面标题: 页面T')).toBe(true)
  })

  it('片段 HTML（无 head/title，Defuddle 产物形态）无标题首行', () => {
    const out = pc.extractPageText('<h2>章节</h2><p>正文</p>')
    expect(out.startsWith('# 页面标题:')).toBe(false)
    expect(out).toContain('## 章节')
  })

  it('表格管道化：行列保留、| 移除、布局表回退（与 Go renderPipeTable 同构）', () => {
    // 标准 thead 表 → 管道行 + 分隔行
    const pipe = pc.extractPageText(
      '<html><body><table><thead><tr><th>功能</th><th>负责人</th></tr></thead><tbody><tr><td>载入地图</td><td>Kovarex</td></tr></tbody></table></body></html>')
    expect(pipe).toContain('| 功能 | 负责人 |\n| --- | --- |\n| 载入地图 | Kovarex |')
    // rowspan 复制填充（A1b）
    const span = pc.extractPageText(
      '<html><body><table><tr><th>组</th><th>项</th></tr><tr><td rowspan="2">共享组</td><td>项1</td></tr><tr><td>项2</td></tr></table></body></html>')
    expect(span).toContain('| 共享组 | 项1 |')
    expect(span).toContain('| 共享组 | 项2 |')
    // 单元格 | 移除
    const bar = pc.extractPageText('<html><body><table><tr><th>表达式</th></tr><tr><td>a || b</td></tr></table></body></html>')
    expect(bar).toContain('| a  b |')
    expect(bar).not.toContain('a || b')
    // 布局表格回退散行（A4a）
    const layout = pc.extractPageText(
      '<html><body><table><tr><td><p>布局段落一。</p></td><td><p>布局段落二。</p></td></tr></table></body></html>')
    expect(layout).not.toContain('| ')
    expect(layout).toContain('布局段落一。')
  })
})

describe('pageconvert.extractPageText — 图片占位（与 Go pageextract_img_test 同构）', () => {
  const wrap = (body: string) => `<html><body>${body}</body></html>`

  it('基本占位：alt + src → ![alt](src)', () => {
    expect(pc.extractPageText(wrap(`<img src="https://x.com/a.png" alt="架构图">`)))
      .toContain('![架构图](https://x.com/a.png)')
  })

  it('alt 缺失回退 title，再回退文件名，全空回退「图片」', () => {
    expect(pc.extractPageText(wrap(`<img src="https://x.com/a.png" title="流程图">`)))
      .toContain('![流程图](https://x.com/a.png)')
    expect(pc.extractPageText(wrap(`<img src="https://x.com/img/diagram-v2.png">`)))
      .toContain('![diagram-v2.png](https://x.com/img/diagram-v2.png)')
    expect(pc.extractPageText(wrap(`<img src="https://x.com/img/">`)))
      .toContain('![图片](https://x.com/img/)')
  })

  it('懒加载：data-src / data-original 优先于 src 占位图', () => {
    expect(pc.extractPageText(wrap(`<img src="loading.gif" data-src="https://x.com/real.png" alt="真实图">`)))
      .toContain('![真实图](https://x.com/real.png)')
    expect(pc.extractPageText(wrap(`<img data-original="https://x.com/lazy.png" alt="懒">`)))
      .toContain('![懒](https://x.com/lazy.png)')
  })

  it('跳过类：data-uri / javascript: / 无 src 不产生占位', () => {
    const out = pc.extractPageText(wrap(
      `<img src="data:image/png;base64,iVBOR" alt="内联图">` +
      `<img src="javascript:void(0)" alt="js">` +
      `<img alt="无src">`))
    expect(out).not.toContain('![')
  })

  it('装饰图启发式：无 alt 且路径命中 icon/logo/sprite/spacer → 跳过；有 alt 保留', () => {
    const out = pc.extractPageText(wrap(
      `<img src="https://x.com/static/icon.png">` +
      `<img src="https://x.com/static/logo.png">` +
      `<img src="https://x.com/static/icon.png" alt="图标说明">`))
    // 无 alt 的两张不产占位（无标签形态与文件名回退形态都不出现）
    expect(out).not.toContain('![](https://x.com/static/')
    expect(out).not.toContain('![icon.png]')
    expect(out).not.toContain('![logo.png]')
    expect(out).toContain('![图标说明](https://x.com/static/icon.png)')
  })

  it('宽高属性齐全时追加 |宽x高，缺一不追加', () => {
    expect(pc.extractPageText(wrap(`<img src="https://x.com/a.png" alt="架构图" width="1200" height="800">`)))
      .toContain('![架构图|1200x800](https://x.com/a.png)')
    expect(pc.extractPageText(wrap(`<img src="https://x.com/a.png" alt="架构图" width="1200">`)))
      .toContain('![架构图](https://x.com/a.png)')
  })

  it('段落内行内占位不破坏文本流', () => {
    expect(pc.extractPageText(wrap(`<p>前文<img src="https://x.com/a.png" alt="图">后文</p>`)))
      .toContain('前文![图](https://x.com/a.png)后文')
  })

  it('长文件名/长 URL 截断（60/300 rune）', () => {
    expect(pc.extractPageText(wrap(`<img src="https://x.com/img/${'a'.repeat(80)}.png">`)))
      .toContain(`![${'a'.repeat(60)}…](https://x.com/img/${'a'.repeat(80)}.png)`)
    const longSrc = `https://x.com/a.png?x=${'b'.repeat(400)}`
    const out = pc.extractPageText(wrap(`<img src="${longSrc}" alt="长">`))
    expect(out).toContain('…)')
    expect(out.length).toBeLessThan(600)
  })

  it('单页占位行上限 20，超出补「（另省略 N 张图）」', () => {
    const imgs = Array.from({ length: 25 }, (_, i) =>
      `<img src="https://x.com/p${i}.png" alt="图${i}">`).join('')
    const out = pc.extractPageText(wrap(imgs))
    expect((out.match(/!\[/g) || []).length).toBe(20)
    expect(out).toContain('（另省略 5 张图）')
  })

  it('figure 内 img 与 figcaption 共存', () => {
    const out = pc.extractPageText(wrap(
      `<figure><img src="https://x.com/arch.png" alt="架构图"><figcaption>系统架构示意</figcaption></figure>`))
    expect(out).toContain('![架构图](https://x.com/arch.png)')
    expect(out).toContain('系统架构示意')
  })
})

// 语料库回归：docs/page-corpus 真实页面快照（本地开发资产，docs/ 不入库，缺失时整组跳过）。
// 断言口径与 Go 端 corpus_test.go 一致：转换非空/壳页极薄、mustContain 存在；
// mdChars 逐字符同构复验在 Go 端做（检索以 Go 端为准）。
describe('pageconvert — page-corpus 语料回归', () => {
  const corpusRoot = join(here, '../../../docs/page-corpus')

  const corpus = exists(corpusRoot)
    ? readdirSync(corpusRoot, { withFileTypes: true }).flatMap((typeEnt) => {
        if (!typeEnt.isDirectory()) return []
        const typeDir = join(corpusRoot, typeEnt.name)
        return readdirSync(typeDir, { withFileTypes: true }).flatMap((pageEnt) => {
          // 只收页面（子目录或 .html 文件），跳过同目录的 meta.json
          if (!pageEnt.isDirectory() && !pageEnt.name.endsWith('.html')) return []
          const htmlPath = pageEnt.isDirectory()
            ? join(typeDir, pageEnt.name, 'source.html')
            : join(typeDir, pageEnt.name)
          if (!exists(htmlPath)) return []
          const metaPath = pageEnt.isDirectory()
            ? join(typeDir, pageEnt.name, 'meta.json')
            : join(typeDir, 'meta.json')
          if (!exists(metaPath)) return []
          const label = `${typeEnt.name}/${pageEnt.name.replace(/\.html$/, '')}`
          return [{ label, html: readFileSync(htmlPath, 'utf8'), meta: JSON.parse(readFileSync(metaPath, 'utf8')) }]
        })
      })
    : []

  it.skipIf(corpus.length === 0)('语料已加载', () => {
    expect(corpus.length).toBeGreaterThan(10)
  })

  for (const { label, html, meta } of corpus) {
    it(`${label}：${meta.expect.nonEmpty ? '正常转换' : '壳页极薄（边界基线）'}`, () => {
      const out = pc.extractPageText(html)
      if (meta.expect.nonEmpty) {
        expect(out.length).toBeGreaterThan(100)
        for (const want of meta.expect.mustContain) {
          // 紧凑匹配（与 pagesearch 一致）：忽略空白与大小写
          expect(out.replace(/[ \t\n\r\f\v]+/g, '').toLowerCase()).toContain(want.replace(/[ \t\n\r\f\v]+/g, '').toLowerCase())
        }
      } else {
        expect(out.length).toBeLessThanOrEqual(100)
      }
    })
  }
})
