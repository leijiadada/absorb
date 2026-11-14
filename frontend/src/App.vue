<script setup lang="ts">
import { onBeforeUnmount, onMounted, provide, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { Boxes, Copy, Download, LayoutDashboard, Minus, Settings as SettingsIcon, Square, X } from 'lucide-vue-next'
import { getStatus, type Status } from './lib/api'
import { useUpdate } from './lib/update'

// 状态轮询：5s 一次，侧栏/顶栏/状态页共用一份数据
const status = ref<Status | null>(null)
let timer: number | undefined

async function refresh() {
  try {
    status.value = await getStatus()
  } catch {
    // 启动初始化期间调用可能失败，等下一轮自愈
  }
}

onMounted(() => {
  refresh()
  timer = window.setInterval(refresh, 5000)
  initUpdate()
})
onBeforeUnmount(() => window.clearInterval(timer))
provide('status', status)

// 升级：全局横幅（任意页面发现新版本时提示）+ 启动订阅事件。
// 设置页自身有更新卡片，横幅在该页隐藏避免重复。
const route = useRoute()
const router = useRouter()
const { view, dismissed, applying, apply, dismiss, initUpdate } = useUpdate()

// ---------- 无边框窗口控制（Frameless，顶栏兼做标题栏） ----------
// window.runtime 由 Wails 注入；测试/浏览器环境缺省时静默跳过。
// 关闭 = 隐藏到托盘（与 main.go HideWindowOnClose 语义一致）。
function winAction(name: 'WindowMinimise' | 'WindowToggleMaximise' | 'WindowHide') {
  const rt = (window as any).runtime
  rt?.[name]?.()
}
const winMinimise = () => winAction('WindowMinimise')
const winToggleMaximise = () => winAction('WindowToggleMaximise')
const winHide = () => winAction('WindowHide')

// 最大化状态跟随 Wails 窗口事件切换图标；事件缺失时图标静止但不影响切换功能
const maximised = ref(false)
onMounted(() => {
  const rt = (window as any).runtime
  rt?.EventsOn?.('window:maximised', () => (maximised.value = true))
  rt?.EventsOn?.('window:unmaximised', () => (maximised.value = false))
})
</script>

<template>
  <div class="flex h-full flex-col">
    <!-- 品牌蓝顶栏：无边框窗口的标题栏——拖拽/双击最大化，右侧窗口控制对齐 Docker Desktop -->
    <header
      class="flex h-12 shrink-0 items-center justify-between bg-primary pl-5"
      style="--wails-draggable: drag"
      @dblclick="winToggleMaximise"
    >
      <div class="flex items-center gap-2">
        <img src="/logo-white.svg" alt="" aria-hidden="true" class="h-5 w-5" />
        <span class="text-[17px] font-semibold tracking-wide text-white">Absorb</span>
        <span class="text-xs text-white/60">阅读助手</span>
      </div>
      <!-- 控制区排除拖拽；双击按钮不应触发标题栏最大化。
           mousedown.prevent：窗口控制按钮不夺取焦点——点关闭隐藏窗口后焦点若留在
           按钮上，从托盘恢复显示时 WebView2 会把焦点还给它，按钮呈选中态 -->
      <div class="flex h-full items-center" style="--wails-draggable: no-drag" @dblclick.stop>
        <span class="mr-1 h-4 w-px bg-white/25" />
        <button class="win-btn" title="最小化" @mousedown.prevent @click="winMinimise">
          <Minus :size="15" />
        </button>
        <button class="win-btn" :title="maximised ? '还原' : '最大化'" @mousedown.prevent @click="winToggleMaximise">
          <Copy v-if="maximised" :size="13" />
          <Square v-else :size="13" />
        </button>
        <button class="win-btn close" title="关闭" @mousedown.prevent @click="winHide">
          <X :size="15" />
        </button>
      </div>
    </header>

    <div class="flex min-h-0 flex-1">
      <aside class="flex w-[220px] shrink-0 flex-col border-r border-brd bg-sidebar">
        <nav class="flex-1 px-3 pt-3">
          <router-link class="nav-item" to="/status">
            <LayoutDashboard :size="16" />
            <span>状态</span>
          </router-link>
          <router-link class="nav-item" to="/models">
            <Boxes :size="16" />
            <span>模型</span>
          </router-link>
          <router-link class="nav-item" to="/settings">
            <SettingsIcon :size="16" />
            <span>设置</span>
          </router-link>
        </nav>
      </aside>

      <div class="flex min-w-0 flex-1 flex-col">
        <main class="min-h-0 flex-1 overflow-y-auto">
          <!-- 全局「更新可用」横幅：启动检查或手动检查发现有新版本时，任意页面顶部提示 -->
          <Transition name="banner">
            <div
              v-if="view.available && !dismissed && route.path !== '/settings'"
              class="flex items-center gap-3 border-b border-brd bg-primary-soft px-5 py-2 text-sm"
            >
              <span class="text-txt">
                发现新版本 v{{ view.latest }}
                <span
                  v-if="view.mandatory"
                  class="ml-2 rounded bg-danger/10 px-1.5 py-0.5 text-xs text-danger"
                  >必须更新</span
                >
              </span>
              <span class="flex-1" />
              <button class="btn btn-ghost" @click="router.push('/settings')">查看</button>
              <button class="btn btn-primary" :disabled="applying" @click="apply()">
                <Download :size="14" class="mr-1" />{{ applying ? '正在更新…' : '立即更新' }}
              </button>
              <button class="icon-btn" title="稍后" @click="dismiss()">
                <X :size="15" />
              </button>
            </div>
          </Transition>

          <div class="mx-auto max-w-[720px] px-6 py-6">
            <!-- 缓存视图实例：切菜单不再卸载重建，避免每次切换闪"加载中"；
                 数据新鲜度：状态页走共享轮询 ref，模型/设置仅本页可改，缓存即最新。
                 若将来出现跨页面数据变更，上提为共享响应式或用 onActivated 静默刷新。 -->
            <router-view v-slot="{ Component }">
              <keep-alive>
                <component :is="Component" />
              </keep-alive>
            </router-view>
          </div>
        </main>
      </div>
    </div>

    <!-- 常驻状态栏（对齐 Docker Desktop 底栏）：服务状态 + 版本与更新入口 -->
    <footer
      class="flex h-8 shrink-0 items-center justify-between border-t border-brd bg-surface px-5 text-xs text-muted"
    >
      <div class="flex items-center gap-2">
        <span
          class="status-dot inline-block h-2 w-2 rounded-full"
          :class="!status ? 'border border-muted' : status.serviceRunning ? 'bg-success' : 'bg-danger'"
        />
        <span>{{ !status ? '正在连接服务…' : status.serviceRunning ? '服务运行中' : '服务未运行' }}</span>
      </div>
      <div class="flex items-center gap-3">
        <!-- 横幅的兜底入口：横幅可见时让位，点过 ✕ 后常驻；设置页由更新卡片接管 -->
        <button
          v-if="view.available && dismissed && route.path !== '/settings'"
          class="text-warning hover:underline"
          @click="router.push('/settings')"
        >
          发现新版本 v{{ view.latest }}
        </button>
        <span v-if="status?.version">{{ status.version.split(' (')[0] }}</span>
      </div>
    </footer>
  </div>
</template>
