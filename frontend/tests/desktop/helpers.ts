// 组件测试共用工具：mock window.go.main.App（Wails 注入的绑定层）+ 挂载用 router。
import { createRouter, createMemoryHistory, type Router } from 'vue-router'
import { flushPromises } from '@vue/test-utils'
import type { Status, Settings, ModelConfigView, TokenUsageStats } from '../../src/lib/api'

export function makeStatus(overrides: Partial<Status> = {}): Status {
  return {
    serviceRunning: true,
    uptime: '2m3s',
    extConnected: false,
    extLastSeen: 0,
    extState: 'never',
    modelName: 'glm-4.6',
    keyConfigured: true,
    proxyUrl: '',
    memoryEnabled: true,
    version: '0.1.0',
    ...overrides,
  }
}

export function makeSettings(overrides: Partial<Settings> = {}): Settings {
  return {
    memoryEnabled: false,
    proxyUrl: 'http://127.0.0.1:7897',
    systemProxy: '127.0.0.1:7897',
    proxyOverride: 'localhost;127.*;<local>',
    configPath: 'C:\\data\\config.yaml',
    dataRoot: 'C:\\data',
    version: '0.1.0',
    ...overrides,
  }
}

export function makeModelConfig(overrides: Partial<ModelConfigView> = {}): ModelConfigView {
  return {
    id: 'mc1',
    provider: 'bigmodel',
    displayName: '',
    modelId: 'glm-4.6',
    baseUrl: 'https://open.bigmodel.cn/api/paas/v4',
    fullUrl: false,
    hasKey: true,
    keyMasked: '…abcd',
    maxTokens: null,
    thinking: 1,
    toolRounds: null,
    imageSupport: true,
    isDefault: true,
    createdAt: '2026-10-05 10:00',
    updatedAt: '2026-10-05 10:00',
    ...overrides,
  }
}

type Handlers = Record<string, (...args: any[]) => any>

// mockBackend 用给定处理器替换 window.go.main.App，未指定的方法返回默认值。
export function mockBackend(h: Handlers = {}) {
  const status = makeStatus()
  const models: ModelConfigView[] = [makeModelConfig()]
  const base: Handlers = {
    GetStatus: () => Promise.resolve(status),
    GetSettings: () => Promise.resolve(makeSettings()),
    SaveSettings: () => Promise.resolve(),
    ListModelConfigs: () => Promise.resolve(models),
    AddModelConfig: () => Promise.resolve(),
    UpdateModelConfig: () => Promise.resolve(),
    DeleteModelConfig: () => Promise.resolve(),
    SetDefaultModel: () => Promise.resolve(),
    TestModelConnection: () => Promise.resolve({ ok: true, latencyMs: 320, error: '' }),
    ExportExtension: () => Promise.resolve({ ok: true, path: 'C:\\ext\\absorb-extension' }),
    OpenExtensionsPage: () => Promise.resolve({ ok: true }),
    GetUpdateStatus: () =>
      Promise.resolve({ current: '0.1.0', available: false, mandatory: false } as any),
    CheckUpdate: () =>
      Promise.resolve({ current: '0.1.0', available: false, mandatory: false } as any),
    ApplyUpdate: () => Promise.resolve(),
  }
  ;(window as any).go = { main: { App: { ...base, ...h } } }
  return { status, models }
}

// testTokenStats 合法的 Token 统计载荷（Token 卡渲染与自动刷新测试用）。
export function testTokenStats(): TokenUsageStats {
  return {
    today: { calls: 3, totalTokens: 12100, promptTokens: 10000, completionTokens: 2100, cachedTokens: 4000 },
    total: { calls: 30, totalTokens: 1234567, promptTokens: 1000000, completionTokens: 234567, cachedTokens: 100000 },
    byKind: { chat: { calls: 20, totalTokens: 1000000, promptTokens: 800000, completionTokens: 200000, cachedTokens: 90000 } },
    cacheHitRate: 0.4,
  }
}

// testRouter 提供 memory history 路由（App.vue / StatusView.vue 含 router-link）。
export function testRouter(): Router {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      // 对齐 src/router.ts 的 '/' → /status 重定向，消除挂载时的路由匹配警告
      { path: '/', redirect: '/status' },
      { path: '/status', component: { template: '<div />' } },
      { path: '/models', component: { template: '<div />' } },
      { path: '/settings', component: { template: '<div />' } },
    ],
  })
}

export async function settle() {
  await flushPromises()
}
