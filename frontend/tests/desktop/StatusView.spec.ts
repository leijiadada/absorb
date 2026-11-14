import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import { mount } from '@vue/test-utils'
import { defineComponent, h, KeepAlive, nextTick, ref } from 'vue'
import StatusView from '../../src/views/StatusView.vue'
import { makeStatus, mockBackend, settle, testRouter, testTokenStats } from './helpers'

function mountStatus(statusValue: ReturnType<typeof makeStatus> | null) {
  mockBackend()
  const router = testRouter()
  const w = mount(StatusView, {
    global: { plugins: [router], provide: { status: ref(statusValue) } },
  })
  return w
}

beforeEach(() => {
  vi.clearAllMocks()
})

describe('扩展卡四态', () => {
  it('已连接：绿点 + 使用提示', () => {
    const w = mountStatus(makeStatus({ extConnected: true, extState: 'connected' }))
    expect(w.text()).toContain('已连接')
    expect(w.text()).toContain('在网页里划词提问即可使用')
    w.unmount()
  })

  it('断开（浏览器在运行）：重新安装按钮打开引导弹窗', async () => {
    const w = mountStatus(makeStatus({ extState: 'disconnected' }))
    const target = w.findAll('button').find((b) => b.text().includes('重新安装扩展'))
    await target!.trigger('click')
    await settle()
    expect(w.text()).toContain('手动安装扩展')
    w.unmount()
  })

  it('浏览器未运行：中性提示，无安装按钮', () => {
    const w = mountStatus(makeStatus({ extConnected: false, extLastSeen: 1000, extState: 'browser-closed' }))
    expect(w.text()).toContain('浏览器未运行')
    expect(w.text()).toContain('打开浏览器后，扩展会自动连接到这里')
    expect(w.text()).not.toContain('重新安装扩展')
    w.unmount()
  })

  it('从未连接：安装按钮打开引导弹窗（下载/打开浏览器分离）', async () => {
    const w = mountStatus(makeStatus({ extConnected: false, extLastSeen: 0, extState: 'never' }))
    expect(w.text()).toContain('未连接')
    expect(w.text()).toContain('如果未安装浏览器扩展，划词阅读功能无法使用')
    await w.find('.btn-primary').trigger('click')
    await settle()
    expect(w.text()).toContain('手动安装扩展')
    expect(w.text()).toContain('下载扩展')
    expect(w.text()).toContain('拖拽安装')
    expect(w.text()).toContain('打开浏览器')
    expect(w.text()).toContain('edge://extensions')
    w.unmount()
  })

  it('弹窗内下载：成功静默（无提示、无路径），失败才报错', async () => {
    const w = mountStatus(makeStatus({ extState: 'never' }))
    await w.find('.btn-primary').trigger('click')
    await w.findAll('button').find((b) => b.text().includes('点击下载'))!.trigger('click')
    await settle()
    expect(w.text()).not.toContain('已下载')
    w.unmount()
  })
})

describe('模型卡三态', () => {
  it('已就绪：绿点 + 当前模型名', () => {
    const w = mountStatus(makeStatus({ keyConfigured: true, modelName: 'glm-4.6' }))
    expect(w.text()).toContain('已就绪')
    expect(w.text()).toContain('当前模型：glm-4.6')
    w.unmount()
  })

  it('有模型缺密钥：琥珀点 + 去编辑链接指向模型页', () => {
    const w = mountStatus(makeStatus({ keyConfigured: false, modelName: 'glm-4.6' }))
    expect(w.text()).toContain('密钥未配置')
    const link = w.findAll('a').find((a) => a.text().includes('去编辑'))
    expect(link).toBeTruthy()
    expect(link!.attributes('href')).toBe('/models')
    w.unmount()
  })

  it('无可用模型：琥珀点 + 去添加链接指向模型页', () => {
    const w = mountStatus(makeStatus({ keyConfigured: false, modelName: '' }))
    expect(w.text()).toContain('还没有可用模型')
    const link = w.findAll('a').find((a) => a.text().includes('去添加'))
    expect(link).toBeTruthy()
    expect(link!.attributes('href')).toBe('/models')
    w.unmount()
  })
})

describe('英雄横幅', () => {
  it('全绿状态：横幅显示一切正常与服务运行时长', () => {
    const w = mountStatus(makeStatus({ serviceRunning: true, extConnected: true, extState: 'connected', uptime: '2m3s', memoryEnabled: true, proxyUrl: '', version: '0.1.0' }))
    expect(w.text()).toContain('一切正常')
    expect(w.text()).toContain('服务运行 2m3s')
    expect(w.text()).not.toContain('运行信息') // 审计卡已移除，页面只留结论层+决策层
    w.unmount()
  })

  it('服务停止：横幅变红提示服务未运行', () => {
    const w = mountStatus(makeStatus({ serviceRunning: false }))
    expect(w.text()).toContain('服务未运行')
    expect(w.text()).toContain('可尝试重启应用')
    w.unmount()
  })

  it('extLastSeen 为 0 显示从未；非 0 显示相对时间（异常横幅内）', () => {
    const w = mountStatus(makeStatus({ extState: 'disconnected', extLastSeen: 0 }))
    expect(w.text()).toContain('从未')
    w.unmount()
    const now = Math.floor(Date.now() / 1000)
    const w2 = mountStatus(makeStatus({ extState: 'disconnected', extLastSeen: now - 300 }))
    expect(w2.text()).toContain('5 分钟前')
    w2.unmount()
  })
})

describe('Token 卡自动刷新', () => {
  // mockBackend 未配置 GetTokenStats 时组件走 catch 静默，这里用计数器验证调用时机
  let calls = 0
  const mountWithStats = (statusValue: ReturnType<typeof makeStatus> | null = null) => {
    calls = 0
    mockBackend({
      GetTokenStats: () => {
        calls++
        return Promise.resolve(testTokenStats())
      },
    })
    const router = testRouter()
    return mount(StatusView, {
      global: { plugins: [router], provide: { status: ref(statusValue) } },
    })
  }

  beforeEach(() => vi.useFakeTimers())
  afterEach(() => {
    vi.useRealTimers()
    vi.clearAllMocks()
  })

  it('挂载刷新一次，停留 5/10 分钟各自动刷新一次', async () => {
    const w = mountWithStats()
    await settle()
    expect(calls).toBe(1)
    await vi.advanceTimersByTimeAsync(5 * 60 * 1000)
    expect(calls).toBe(2)
    await vi.advanceTimersByTimeAsync(5 * 60 * 1000)
    expect(calls).toBe(3)
    w.unmount()
  })

  it('keep-alive 切走停表，切回立即刷新且定时器重启', async () => {
    calls = 0
    const show = ref(true)
    const router = testRouter()
    // 用 render 函数而非 template 字符串：vitest 的 vue 构建不含运行时编译器
    const host = defineComponent({
      setup: () => () => h(KeepAlive, () => (show.value ? h(StatusView) : null)),
    })
    const w = mount(host, { global: { plugins: [router], provide: { status: ref(null) } } })
    await settle()
    // keep-alive 内挂载：onMounted 与 onActivated 各刷一次（与生产行为一致）
    expect(calls).toBe(2)

    show.value = false // 切走 → onDeactivated 停表
    await nextTick()
    await vi.advanceTimersByTimeAsync(10 * 60 * 1000)
    expect(calls).toBe(2) // 定时器已停，不再调用

    show.value = true // 切回 → onActivated 立即刷新并重启定时器
    await nextTick()
    await settle()
    expect(calls).toBe(3)
    await vi.advanceTimersByTimeAsync(5 * 60 * 1000)
    expect(calls).toBe(4)
    w.unmount()
  })

  it('手动刷新按钮即时拉取', async () => {
    const w = mountWithStats()
    await settle()
    expect(calls).toBe(1)
    const btn = w.findAll('button').find((b) => b.attributes('title') === '刷新')!
    await btn.trigger('click')
    await settle()
    expect(calls).toBe(2)
    w.unmount()
  })
})
