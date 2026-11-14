import { describe, expect, it, vi, beforeEach } from 'vitest'
import { mount } from '@vue/test-utils'
import App from '../../src/App.vue'
import { makeStatus, mockBackend, settle, testRouter } from './helpers'

beforeEach(() => {
  localStorage.clear()
})

describe('App 布局与状态', () => {
  it('渲染三导航且指向正确路由', async () => {
    mockBackend()
    const router = testRouter()
    const w = mount(App, { global: { plugins: [router] } })
    await router.isReady()
    await settle()
    const links = w.findAll('a.nav-item')
    expect(links).toHaveLength(3)
    expect(links[0].attributes('href')).toBe('/status')
    expect(links[1].attributes('href')).toBe('/models')
    expect(links[2].attributes('href')).toBe('/settings')
    w.unmount()
  })

  it('status 为 null 时状态栏显示中性"正在连接服务…"（不闪红）', async () => {
    mockBackend({ GetStatus: () => new Promise(() => {}) }) // 永不返回 → status 保持 null
    const router = testRouter()
    const w = mount(App, { global: { plugins: [router] } })
    await router.isReady()
    await settle()
    expect(w.text()).toContain('正在连接服务…')
    expect(w.find('footer .inline-block').classes()).toContain('border')
    expect(w.text()).not.toContain('服务未运行')
    w.unmount()
  })

  it('服务运行失败时状态栏红点 + 文案', async () => {
    mockBackend({ GetStatus: () => Promise.resolve(makeStatus({ serviceRunning: false })) })
    const router = testRouter()
    const w = mount(App, { global: { plugins: [router] } })
    await router.isReady()
    await settle()
    expect(w.text()).toContain('服务未运行')
    expect(w.find('footer .inline-block').classes()).toContain('bg-danger')
    w.unmount()
  })
})

describe('无边框窗口控制', () => {
  it('最小化/最大化/关闭分别调用对应 runtime 方法（关闭=隐藏到托盘）', async () => {
    const calls: string[] = []
    ;(window as any).runtime = {
      WindowMinimise: () => calls.push('min'),
      WindowToggleMaximise: () => calls.push('max'),
      WindowHide: () => calls.push('hide'),
    }
    mockBackend()
    const router = testRouter()
    const w = mount(App, { global: { plugins: [router] } })
    await router.isReady()
    await settle()
    await w.find('button[title="最小化"]').trigger('click')
    await w.find('button[title="最大化"]').trigger('click')
    await w.find('button[title="关闭"]').trigger('click')
    expect(calls).toEqual(['min', 'max', 'hide'])
    w.unmount()
    delete (window as any).runtime
  })

  it('runtime 缺失（测试/浏览器环境）时点击不报错', async () => {
    delete (window as any).runtime
    mockBackend()
    const router = testRouter()
    const w = mount(App, { global: { plugins: [router] } })
    await router.isReady()
    await settle()
    await w.find('button[title="关闭"]').trigger('click')
    await w.find('header').trigger('dblclick')
    w.unmount()
  })
})
