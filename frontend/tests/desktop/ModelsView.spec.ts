import { describe, expect, it, vi, beforeEach } from 'vitest'
import { mount } from '@vue/test-utils'
import ModelsView from '../../src/views/ModelsView.vue'
import { makeModelConfig, mockBackend, settle } from './helpers'

beforeEach(() => {
  vi.clearAllMocks()
})

async function mountModels(handlers = {}, models = undefined as any) {
  const h = { ...handlers }
  if (models !== undefined) h.ListModelConfigs = () => Promise.resolve(models)
  mockBackend(h)
  const w = mount(ModelsView)
  await settle()
  return w
}

describe('列表渲染与空态', () => {
  it('渲染模型行：默认行绿点+当前使用，服务商列显示名称，密钥列展示配置状态', async () => {
    const w = await mountModels({}, [
      makeModelConfig({ id: 'mc1', modelId: 'glm-4.6', provider: 'bigmodel', isDefault: true }),
      makeModelConfig({ id: 'mc2', modelId: 'deepseek-chat', provider: 'deepseek', isDefault: false, thinking: 0 }),
    ])
    const rows = w.findAll('tbody tr')
    expect(rows).toHaveLength(2)
    expect(w.text()).toContain('glm-4.6')
    expect(w.text()).toContain('当前使用')
    expect(w.text()).toContain('Bigmodel 智谱')
    expect(w.text()).toContain('DeepSeek')
    expect(w.text()).toContain('已配置')
    const dots = w.findAll('tbody tr span.inline-block')
    expect(dots[0].classes()).toContain('bg-success')
    expect(dots[1].classes()).not.toContain('bg-success')
    w.unmount()
  })

  it('空态引导文案', async () => {
    const w = await mountModels({}, [])
    expect(w.text()).toContain('还没有可用模型')
    expect(w.text()).toContain('添加模型')
    w.unmount()
  })
})

describe('两步添加弹窗', () => {
  it('添加 → 服务商网格 → 选中预填进入表单', async () => {
    const w = await mountModels()
    await w.find('button.btn-primary').trigger('click')
    expect(w.text()).toContain('选择服务商')
    expect(w.text()).toContain('Bigmodel 智谱')
    expect(w.text()).toContain('自定义模型')
    const grid = w.findAll('.grid button')
    const deepseek = grid.find((b) => b.text().includes('DeepSeek'))!
    await deepseek.trigger('click')
    await settle()
    // 表单预填：服务商只读、模型 ID 取第一个预置（按 placeholder/readonly 语义定位）
    const providerInput = w.findAll('input').find((i) => (i.element as HTMLInputElement).readOnly)!
    expect((providerInput.element as HTMLInputElement).value).toBe('DeepSeek')
    const modelInput = w.find('input[placeholder="输入或选择模型 ID"]')
    expect((modelInput.element as HTMLInputElement).value).toBe('deepseek-flash')
    await w.findAll('button').find((b) => b.text().includes('高级配置'))!.trigger('click')
    const advInput = w.find('input[placeholder="https://…"]')
    expect((advInput.element as HTMLInputElement).value).toContain('api.deepseek.com')
    w.unmount()
  })

  it('校验：模型 ID 为空时提示且不提交', async () => {
    const add = vi.fn().mockResolvedValue(undefined)
    const w = await mountModels({ AddModelConfig: add })
    await w.find('button.btn-primary').trigger('click')
    await w.findAll('.grid button').find((b) => b.text().includes('自定义模型'))!.trigger('click')
    await settle()
    const modelInput = w.find('input[placeholder="输入或选择模型 ID"]')
    ;(modelInput.element as HTMLInputElement).value = ''
    await modelInput.trigger('input')
    await w.findAll('button').find((b) => b.text() === '添加')!.trigger('click')
    await settle()
    expect(add).not.toHaveBeenCalled()
    expect(w.text()).toContain('请填写模型 ID')
    w.unmount()
  })

  it('提交调用 AddModelConfig 且关闭弹窗', async () => {
    const add = vi.fn().mockResolvedValue(undefined)
    const w = await mountModels({ AddModelConfig: add })
    await w.find('button.btn-primary').trigger('click')
    await w.findAll('.grid button').find((b) => b.text().includes('DeepSeek'))!.trigger('click')
    await settle()
    const keyInput = w.find('input[placeholder="粘贴服务商的 API 密钥"]')
    ;(keyInput.element as HTMLInputElement).value = 'sk-test-123'
    await keyInput.trigger('input')
    await w.findAll('button').find((b) => b.text() === '添加')!.trigger('click')
    await settle()
    expect(add).toHaveBeenCalledTimes(1)
    const arg = add.mock.calls[0][0]
    expect(arg.provider).toBe('deepseek')
    expect(arg.modelId).toBe('deepseek-flash')
    expect(arg.apiKey).toBe('sk-test-123')
    expect(w.text()).not.toContain('选择服务商')
    w.unmount()
  })
})

describe('编辑 Key 留空语义', () => {
  it('编辑回填且 Key 为空；提交时 apiKey 为空串（后端保留原值）', async () => {
    const update = vi.fn().mockResolvedValue(undefined)
    const w = await mountModels({ UpdateModelConfig: update })
    await w.find('button[title="编辑"]').trigger('click')
    await settle()
    // Key 输入按动态 placeholder 语义定位（编辑态含"留空保持不变"）
    const keyInput = w.find('input[placeholder*="留空保持不变"]')
    expect((keyInput.element as HTMLInputElement).value).toBe('')
    expect((keyInput.element as HTMLInputElement).placeholder).toContain('留空保持不变')
    const modelInput = w.find('input[placeholder="输入或选择模型 ID"]')
    ;(modelInput.element as HTMLInputElement).value = 'glm-4.5-air'
    await modelInput.trigger('input')
    await w.findAll('button').find((b) => b.text() === '保存')!.trigger('click')
    await settle()
    expect(update).toHaveBeenCalledTimes(1)
    const arg = update.mock.calls[0][0]
    expect(arg.id).toBe('mc1')
    expect(arg.modelId).toBe('glm-4.5-air')
    expect(arg.apiKey).toBe('')
    w.unmount()
  })
})

describe('高级配置（Trae 对齐）', () => {
  async function openAdv(w: any) {
    await w.find('button[title="编辑"]').trigger('click')
    await settle()
    await w.findAll('button').find((b) => b.text().includes('高级配置'))!.trigger('click')
  }

  it('编辑回填：思考模式三态 radio、工具轮数、图片输入', async () => {
    const w = await mountModels({}, [makeModelConfig({ thinking: 2, toolRounds: 8, imageSupport: false })])
    await openAdv(w)
    const thinking = w.findAll('input[name="thinking"]')
    expect(thinking).toHaveLength(3)
    expect((thinking[0].element as HTMLInputElement).checked).toBe(false)
    expect((thinking[2].element as HTMLInputElement).checked).toBe(true)
    const img = w.findAll('input[name="img-support"]')
    expect((img[1].element as HTMLInputElement).checked).toBe(true) // 不支持
    const rounds = w.findAll('input').find((i) => (i.element as HTMLInputElement).value === '8')
    expect(rounds).toBeTruthy()
    w.unmount()
  })

  it('输出快捷档位点击填入输入框', async () => {
    const w = await mountModels()
    await openAdv(w)
    await w.findAll('button').find((b) => b.text() === '32k')!.trigger('click')
    const out = w.findAll('input').find((i) => (i.element as HTMLInputElement).value === '32000')
    expect(out).toBeTruthy()
    w.unmount()
  })

  it('提交 payload 携带三态与新字段', async () => {
    const update = vi.fn().mockResolvedValue(undefined)
    const w = await mountModels({ UpdateModelConfig: update }, [
      makeModelConfig({ thinking: 1, toolRounds: 8, imageSupport: false }),
    ])
    await openAdv(w)
    await w.findAll('button').find((b) => b.text() === '保存')!.trigger('click')
    await settle()
    const arg = update.mock.calls[0][0]
    expect(arg.thinking).toBe(1)
    expect(arg.toolRounds).toBe(8)
    expect(arg.imageSupport).toBe(false)
    expect(arg.temperature).toBeUndefined()
    w.unmount()
  })
})

describe('删除确认', () => {
  it('删除需二次确认，确认后调用 DeleteModelConfig', async () => {
    const del = vi.fn().mockResolvedValue(undefined)
    const w = await mountModels({ DeleteModelConfig: del })
    await w.find('button[title="删除"]').trigger('click')
    expect(w.text()).toContain('删除这个模型？')
    await w.findAll('button').find((b) => b.text() === '取消')!.trigger('click')
    expect(del).not.toHaveBeenCalled()
    await w.find('button[title="删除"]').trigger('click')
    await w.findAll('button').find((b) => b.text() === '删除')!.trigger('click')
    await settle()
    expect(del).toHaveBeenCalledWith('mc1')
    w.unmount()
  })
})

describe('切换默认', () => {
  it('点击非默认行调用 SetDefaultModel，点击默认行不调用', async () => {
    const setDefault = vi.fn().mockResolvedValue(undefined)
    const w = await mountModels({ SetDefaultModel: setDefault }, [
      makeModelConfig({ id: 'mc1', isDefault: true }),
      makeModelConfig({ id: 'mc2', modelId: 'deepseek-chat', provider: 'deepseek', isDefault: false }),
    ])
    const rows = w.findAll('tbody tr')
    await rows[1].trigger('click')
    expect(setDefault).toHaveBeenCalledWith('mc2')
    await rows[0].trigger('click')
    expect(setDefault).toHaveBeenCalledTimes(1)
    w.unmount()
  })
})

describe('连通性测试三态', () => {
  it('成功：绿色提示带延迟', async () => {
    const test = vi.fn().mockResolvedValue({ ok: true, latencyMs: 320, error: '' })
    const w = await mountModels({ TestModelConnection: test })
    await w.find('button[title="编辑"]').trigger('click')
    await settle()
    await w.findAll('button').find((b) => b.text() === '测试')!.trigger('click')
    await settle()
    expect(test).toHaveBeenCalledTimes(1)
    expect(w.text()).toContain('连接成功 · 320ms')
    w.unmount()
  })

  it('失败：红色人话错误', async () => {
    const test = vi.fn().mockResolvedValue({ ok: false, latencyMs: 88, error: 'API 密钥无效或未授权，请检查密钥' })
    const w = await mountModels({ TestModelConnection: test })
    await w.find('button[title="编辑"]').trigger('click')
    await settle()
    await w.findAll('button').find((b) => b.text() === '测试')!.trigger('click')
    await settle()
    expect(w.text()).toContain('API 密钥无效或未授权')
    w.unmount()
  })

  it('测试中按钮文案变化', async () => {
    let resolve!: (v: any) => void
    const test = vi.fn().mockReturnValue(new Promise((r) => (resolve = r)))
    const w = await mountModels({ TestModelConnection: test })
    await w.find('button[title="编辑"]').trigger('click')
    await settle()
    await w.findAll('button').find((b) => b.text() === '测试')!.trigger('click')
    expect(w.text()).toContain('测试中…')
    resolve({ ok: true, latencyMs: 10, error: '' })
    await settle()
    expect(w.text()).toContain('连接成功')
    w.unmount()
  })
})
