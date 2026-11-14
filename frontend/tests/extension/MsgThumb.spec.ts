import { describe, it, expect, vi, beforeAll, beforeEach } from 'vitest'

// render.js 顶层初始化依赖 sidepanel.html <script> 注入的全局 marked，jsdom 无此
// 全局 → 须在动态 import 前 stub（静态 import 会被提升到 stub 之前而失败）。
vi.stubGlobal('marked', { setOptions: vi.fn(), use: vi.fn(), parse: () => '' })

let buildMsgNode: (role: string, content: string, streaming?: boolean,
  context?: unknown, reasoning?: unknown, stopped?: boolean, images?: unknown) => HTMLElement
beforeAll(async () => {
  ;({ buildMsgNode } = await import('../../../extension/sidepanel/js/render.js'))
  // showToast 依赖 sidepanel.html 的 #toast 容器，jsdom 补一个
  const toast = document.createElement('div')
  toast.id = 'toast'
  document.body.appendChild(toast)
})

// lightbox 单例跨用例残留：清掉上一用例打开的位图，保证断言只看本用例
beforeEach(() => {
  document.querySelectorAll('.mermaid-lightbox .lightbox-img').forEach((n) => n.remove())
})

// 方案 D（design/image-pipeline-plan.md）：用户气泡渲染历史缩略图预览。
// 旧消息（无 thumb）兜底 image-tag 图标——两种形态并存于同一历史列表，均须正确。
describe('buildMsgNode — 历史缩略图渲染（方案 D）', () => {
  it('images 带 thumb 时渲染 .msg-thumb，src 为缩略 dataURL', () => {
    const node = buildMsgNode('user', '看这张图', false, null, null, false,
      [{ w: 800, h: 600, thumb: 'data:image/jpeg;base64,AAAA' }])
    const imgs = node.querySelectorAll('img.msg-thumb')
    expect(imgs.length).toBe(1)
    expect(imgs[0].getAttribute('src')).toBe('data:image/jpeg;base64,AAAA')
  })

  it('images 无 thumb（旧消息）兜底 image-tag 图标，不渲染缩略', () => {
    const node = buildMsgNode('user', '看这张图', false, null, null, false,
      [{ w: 10, h: 10 }])
    expect(node.querySelector('.image-tag')).not.toBeNull()
    expect(node.querySelector('img.msg-thumb')).toBeNull()
  })

  it('多张图渲染多枚缩略', () => {
    const node = buildMsgNode('user', '看这两张', false, null, null, false,
      [{ thumb: 'data:image/jpeg;base64,A' }, { thumb: 'data:image/jpeg;base64,B' }])
    expect(node.querySelectorAll('img.msg-thumb').length).toBe(2)
  })

  it('点击缩略：有 full（内存态原图）时 lightbox 放大原图', () => {
    const node = buildMsgNode('user', '看图', false, null, null, false,
      [{ w: 800, h: 600, thumb: 'data:image/jpeg;base64,T', full: 'data:image/jpeg;base64,F' }])
    ;(node.querySelector('img.msg-thumb') as HTMLElement).click()
    const lb = document.querySelector('.mermaid-lightbox .lightbox-img')
    expect(lb).not.toBeNull()
    expect(lb!.getAttribute('src')).toBe('data:image/jpeg;base64,F')
  })

  it('点击缩略：仅剩 thumb（重开面板后）不开 lightbox，就近气泡提示', () => {
    const node = buildMsgNode('user', '看图', false, null, null, false,
      [{ w: 800, h: 600, thumb: 'data:image/jpeg;base64,T' }])
    const img = node.querySelector('img.msg-thumb') as HTMLElement
    img.click()
    // 不展示糊图误导用户：无 full 时点击只在缩略行正上方弹 data-tip 气泡（不打开
    // lightbox）。提示挂 .msg-thumbs 容器——img 是替换元素，::after 伪元素不渲染
    expect(document.querySelector('.mermaid-lightbox .lightbox-img')).toBeNull()
    const row = node.querySelector('.msg-thumbs') as HTMLElement
    expect(row.dataset.tip).toBe('原图不保留，可重新截图核对')
    expect(row.classList.contains('tip-once')).toBe(true)
  })
})
