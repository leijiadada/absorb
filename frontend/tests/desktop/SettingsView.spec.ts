import { describe, expect, it, vi, beforeEach } from 'vitest'
import { mount } from '@vue/test-utils'
import SettingsView from '../../src/views/SettingsView.vue'
import { mockBackend, testRouter, settle } from './helpers'
import { useUpdate } from '../../src/lib/update'

type Handlers = Record<string, (...args: any[]) => any>

function mountSettings(h: Handlers = {}) {
  mockBackend(h)
  const router = testRouter()
  return mount(SettingsView, { global: { plugins: [router] } })
}

beforeEach(() => {
  vi.clearAllMocks()
  // 升级状态是模块级单例，跨用例共享——每个用例前复位，避免相互污染
  const u = useUpdate()
  u.view.value = { current: '', available: false, mandatory: false }
  u.dismissed.value = false
  u.applying.value = false
  u.progress.value = null
  u.error.value = ''
  u.checking.value = false
})

describe('更新卡片', () => {
  it('展示当前版本与检查按钮', async () => {
    const w = mountSettings()
    await settle()
    expect(w.text()).toContain('更新')
    expect(w.text()).toContain('当前版本')
    expect(w.text()).toContain('0.1.0') // GetUpdateStatus mock 返回
    expect(w.text()).toContain('检查更新')
    w.unmount()
  })

  it('点击检查更新发现新版本：展示详情 + 更新按钮 + 发布说明', async () => {
    const w = mountSettings({
      CheckUpdate: () =>
        Promise.resolve({
          current: '0.1.0',
          latest: '2.0.0',
          available: true,
          mandatory: false,
          notesUrl: 'https://example.com/notes',
        }),
    })
    await settle()
    await w.findAll('button').find((b) => b.text().includes('检查更新'))!.trigger('click')
    await settle()
    expect(w.text()).toContain('发现新版本 v2.0.0')
    expect(w.text()).toContain('下载并更新')
    expect(w.text()).toContain('查看发布说明')
    w.unmount()
  })

  it('必须更新版本：显示"必须更新"标记', async () => {
    const w = mountSettings({
      CheckUpdate: () =>
        Promise.resolve({
          current: '0.1.0',
          latest: '2.0.0',
          available: true,
          mandatory: true,
          notesUrl: '',
        }),
    })
    await settle()
    await w.findAll('button').find((b) => b.text().includes('检查更新'))!.trigger('click')
    await settle()
    expect(w.text()).toContain('必须更新')
    w.unmount()
  })

  it('点击下载并更新：调用后端 ApplyUpdate', async () => {
    const applyMock = vi.fn(() => Promise.resolve())
    const w = mountSettings({
      CheckUpdate: () =>
        Promise.resolve({ current: '0.1.0', latest: '2.0.0', available: true, mandatory: false, notesUrl: '' }),
      ApplyUpdate: applyMock,
    })
    await settle()
    await w.findAll('button').find((b) => b.text().includes('检查更新'))!.trigger('click')
    await settle()
    await w.findAll('button').find((b) => b.text().includes('下载并更新'))!.trigger('click')
    await settle()
    expect(applyMock).toHaveBeenCalledTimes(1)
    w.unmount()
  })

  it('检查失败：展示友好错误且不显示更新面板', async () => {
    const w = mountSettings({ CheckUpdate: () => Promise.reject(new Error('[net_fail] connection refused')) })
    await settle()
    await w.findAll('button').find((b) => b.text().includes('检查更新'))!.trigger('click')
    await settle()
    expect(w.text()).toContain('网络连接失败')
    expect(w.text()).not.toContain('发现新版本')
    expect(w.text()).not.toContain('connection refused')
    w.unmount()
  })
})
