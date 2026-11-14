// 桌面端 Go 绑定访问封装：Wails 运行时把 main.App 的绑定方法注入 window.go.main.App。
// 字段名与 app.go 中各结构体的 json tag 一一对应。

const app = () => (window as any).go.main.App

export interface Status {
  serviceRunning: boolean
  uptime: string
  extConnected: boolean
  extLastSeen: number
  extState: 'connected' | 'disconnected' | 'browser-closed' | 'never'
  modelName: string
  keyConfigured: boolean
  proxyUrl: string
  memoryEnabled: boolean
  version: string
}

export interface ExportResult {
  ok: boolean
  path?: string
  error?: string
}

export interface Settings {
  memoryEnabled: boolean
  proxyUrl: string
  systemProxy: string
  proxyOverride: string
  configPath: string
  dataRoot: string
  version: string
}

export interface ModelConfigView {
  id: string
  provider: string
  displayName: string
  modelId: string
  baseUrl: string
  fullUrl: boolean
  hasKey: boolean
  keyMasked: string // 如 …abcd；无 Key 为空
  maxTokens: number | null
  thinking: number // 0=跟随模型默认 1=开启 2=关闭
  toolRounds: number | null // 工具调用轮数，null=框架默认
  imageSupport: boolean
  isDefault: boolean
  createdAt: string
  updatedAt: string
}

export interface ModelConfigInput {
  id?: string
  provider: string
  displayName?: string
  modelId: string
  baseUrl: string
  fullUrl?: boolean
  apiKey?: string // 编辑时留空 = 保留原 Key
  maxTokens?: number | null
  thinking?: number
  toolRounds?: number | null
  imageSupport?: boolean
}

export interface ModelTestResult {
  ok: boolean
  latencyMs: number
  error: string
}

export const getStatus = (): Promise<Status> => app().GetStatus()
export const getSettings = (): Promise<Settings> => app().GetSettings()
export const saveSettings = (s: Settings): Promise<void> => app().SaveSettings(s)
export const listModelConfigs = (): Promise<ModelConfigView[]> => app().ListModelConfigs()
export const addModelConfig = (m: ModelConfigInput): Promise<void> => app().AddModelConfig(m)
export const updateModelConfig = (m: ModelConfigInput): Promise<void> => app().UpdateModelConfig(m)
export const deleteModelConfig = (id: string): Promise<void> => app().DeleteModelConfig(id)
export const setDefaultModel = (id: string): Promise<void> => app().SetDefaultModel(id)
export const testModelConnection = (m: ModelConfigInput): Promise<ModelTestResult> =>
  app().TestModelConnection(m)
export const exportExtension = (): Promise<ExportResult> => app().ExportExtension()
export const openExtensionsPage = (): Promise<ExportResult> => app().OpenExtensionsPage()

// ---------- Token 用量统计 ----------

// 一组 token 用量合计（今日/全期/按入口共用）
export interface TokenUsageTotals {
  calls: number
  totalTokens: number
  promptTokens: number
  completionTokens: number
  cachedTokens: number
}

export interface TokenUsageStats {
  today: TokenUsageTotals // 本地今日
  total: TokenUsageTotals // 全部累计
  byKind: Record<string, TokenUsageTotals> // 按入口全期分组（chat/draft/summary），不存在的无键
  cacheHitRate: number // 今日缓存命中率 cached/prompt（0-1）
}

export const getTokenStats = (): Promise<TokenUsageStats> => app().GetTokenStats()

// ---------- 升级 ----------

export interface UpdateView {
  current: string // 当前版本（含构建时间）
  latest?: string // feed 中的最新版本
  available: boolean // 有新版本可升级
  mandatory: boolean // 低于最低要求版本，必须升级
  notesUrl?: string // Release Notes 链接
  message?: string // 人类可读状态说明
}

// 最近一次检查结论（不触发网络），供展示与轮询。
export const getUpdateStatus = (): Promise<UpdateView> => app().GetUpdateStatus()
// 手动检查更新（「检查更新」按钮）；已是最新时 err 为 nil，结论在 message。
export const checkUpdate = (): Promise<UpdateView> => app().CheckUpdate()
// 下载并安装当前已发现的更新（需先 checkUpdate 或启动检查已发现新版本）。
export const applyUpdate = (): Promise<void> => app().ApplyUpdate()
