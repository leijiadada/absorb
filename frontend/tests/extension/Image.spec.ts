import { describe, it, expect, vi, beforeAll } from 'vitest'

// render.js 顶层初始化依赖 sidepanel.html <script> 注入的全局 marked，jsdom 无此
// 全局 → 须在动态 import 前 stub（静态 import 会被提升到 stub 之前而失败）。
vi.stubGlobal('marked', { setOptions: vi.fn(), use: vi.fn(), parse: () => '' })

let COMPRESS_PRESET: Record<string, { maxW: number; quality: number }>
beforeAll(async () => {
  ;({ COMPRESS_PRESET } = await import('../../../extension/sidepanel/js/image.js'))
})

// 方案 C（design/image-pipeline-plan.md）：压缩参数按来源分流，三处调用点
// （viewport / crop / upload）共用此表——误改或误删会静默改变所有截图/上传的
// 体积与清晰度，此处常量断言防参数漂移。compressImage 本体依赖 canvas 绘制，
// jsdom 无法执行，其行为（小图不放大）由手动验收覆盖。
describe('COMPRESS_PRESET — 压缩分流预设（方案 C）', () => {
  it('三键齐全：crop / viewport / upload（区域与元素截图共用 crop 路径）', () => {
    expect(Object.keys(COMPRESS_PRESET).sort()).toEqual(['crop', 'upload', 'viewport'])
  })

  it('区域/元素截图：文字信息密集，质量优先 0.9', () => {
    expect(COMPRESS_PRESET.crop).toEqual({ maxW: 1280, quality: 0.9 })
  })

  it('视口截图：布局参考为主，允许更激进压缩 0.8', () => {
    expect(COMPRESS_PRESET.viewport).toEqual({ maxW: 1280, quality: 0.8 })
  })

  it('上传：尊重原图，宽上限放宽到 1600', () => {
    expect(COMPRESS_PRESET.upload).toEqual({ maxW: 1600, quality: 0.85 })
  })
})
