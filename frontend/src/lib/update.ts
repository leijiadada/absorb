// 升级状态的跨组件单一来源：设置页的更新面板与 App.vue 的全局横幅共享同一份响应式
// 状态，避免重复拉取与重复订阅。模块级单例——无论多少个组件调用 useUpdate()，
// 事件只订阅一次、状态只有一份。
//
// 后端（app.go）在启动时后台调用 upgrade.Service.CheckAndNotify：发现新版本经 Wails
// 事件 absorb:update-available 推来；ApplyUpdate 下载过程中经 absorb:update-progress 推
// 下载百分比。本模块订阅这两个事件并维护状态。
import { ref, computed, type Ref } from 'vue'
import { getUpdateStatus, checkUpdate, applyUpdate, type UpdateView } from './api'
import { mapUpdateError, mapUpdateMessage, type Friendly } from './messages'

const view: Ref<UpdateView> = ref<UpdateView>({ current: '', available: false, mandatory: false })
const checking = ref(false)
const applying = ref(false)
const progress = ref<number | null>(null)
const error = ref<Friendly | null>(null) // 检查/更新失败时的友好错误
const dismissed = ref(false) // 全局横幅被用户手动关闭

let subscribed = false

function openExternal(url: string) {
  const rt = (window as any).runtime
  if (rt?.BrowserOpenURL) rt.BrowserOpenURL(url)
}

// 订阅 Wails 升级事件。幂等：多次调用只生效一次。运行环境无 window.runtime（如单测）
// 时安全跳过。
function subscribe() {
  if (subscribed) return
  subscribed = true
  const rt = (window as any).runtime
  if (!rt?.EventsOn) return
  rt.EventsOn('absorb:update-available', (p: any) => {
    view.value = { ...view.value, available: true, latest: p?.version ?? view.value.latest }
    // 事件负载不含 notesUrl/mandatory，回填以 LastResult 的完整视图
    void refresh()
  })
  rt.EventsOn('absorb:update-progress', (p: any) => {
    applying.value = true
    progress.value = typeof p?.percent === 'number' ? p.percent : progress.value
  })
}

export async function refresh() {
  try {
    view.value = await getUpdateStatus()
  } catch {
    // 启动初始化期间可能失败，忽略——下次轮询/打开设置页自愈
  }
}

export async function check() {
  if (checking.value) return
  checking.value = true
  error.value = null
  try {
    view.value = await checkUpdate()
  } catch (e) {
    error.value = mapUpdateError(e)
  } finally {
    checking.value = false
  }
}

export async function apply() {
  if (applying.value) return
  applying.value = true
  progress.value = 0
  error.value = null
  try {
    await applyUpdate()
    // 成功：安装器已拉起，应用将重启；进度由 absorb:update-progress 继续驱动，
    // 此处不复位 applying，保持"正在更新"直到进程结束。
  } catch (e) {
    applying.value = false
    progress.value = null
    error.value = mapUpdateError(e)
  }
}

// 后端经 view.message 透出的结论（含已编码的错误）。有 code 则转友好文案，否则原样
// 作为普通提示（如"已是最新版本"）——与 error（红字）区分：error 优先。
const statusMessage = computed<Friendly | null>(() => {
  const m = view.value.message
  if (!m) return null
  return mapUpdateMessage(m) ?? { level: 'info', title: '', detail: m }
})

export function dismiss() {
  dismissed.value = true
}

// 应用启动处（App.vue onMounted）调用一次：订阅事件 + 拉取最近结论。
export function initUpdate() {
  subscribe()
  void refresh()
}

export function useUpdate() {
  return {
    view,
    checking,
    applying,
    progress,
    error,
    dismissed,
    statusMessage,
    refresh,
    check,
    apply,
    dismiss,
    initUpdate,
    openExternal,
  }
}
