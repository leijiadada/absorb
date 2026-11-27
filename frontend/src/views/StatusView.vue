<script setup lang="ts">
import { computed, inject, onActivated, onDeactivated, onMounted, onUnmounted, ref, type Ref } from 'vue'
import { Check, Copy, Download, ExternalLink, X, ArrowRight, RefreshCw } from 'lucide-vue-next'
import type { Status, TokenUsageStats } from '../lib/api'
import { exportExtension, getTokenStats, openExtensionsPage } from '../lib/api'
import { sanitizeError } from '../lib/messages'

const status = inject<Ref<Status | null>>('status')

// ---------- Token 用量概览（telemetry 第二批） ----------
// 旁路统计：挂载/切回状态页时刷新一次，页面停留期间每 5 分钟自动刷新，卡片提供手动刷新；加载失败静默保留旧数据。
const tokenStats = ref<TokenUsageStats | null>(null)

async function refreshTokenStats() {
  try {
    tokenStats.value = await getTokenStats()
  } catch {
    // 统计加载失败不打扰页面（保留上次数据或维持隐藏）
  }
}

// 自动刷新定时器：仅状态页在前台（keep-alive 激活）时运行，切走即停
const REFRESH_MS = 5 * 60 * 1000
let timer: number | undefined

function startTimer() {
  if (timer === undefined) timer = window.setInterval(refreshTokenStats, REFRESH_MS)
}
function stopTimer() {
  if (timer !== undefined) {
    clearInterval(timer)
    timer = undefined
  }
}

onMounted(() => {
  refreshTokenStats()
  startTimer()
})
// keep-alive 缓存视图实例，onMounted 只跑一次；切回状态页时经 onActivated 立即刷新
onActivated(() => {
  refreshTokenStats()
  startTimer()
})
onDeactivated(stopTimer)
onUnmounted(stopTimer)

// token 数格式化：千分位完整数字（与 DeepSeek 等平台用量页一致），不做 k/m 缩写
function fmtTokens(n: number): string {
  return n.toLocaleString('en-US')
}

// 今日缓存命中率百分比（0-1 → 整数百分数）
const cacheHitPct = computed(() => Math.round((tokenStats.value?.cacheHitRate ?? 0) * 100))

// 扩展连接检查窗口：桌面端刚启动时心跳可能尚未到达（never ≠ 未安装），先展示
// "检查中…"；10s 后仍无心跳才按未安装展示安装引导。窗口期内状态切走立即退出。
const CHECK_MS = 10_000
const checkDone = ref(false)
onMounted(() => {
  window.setTimeout(() => (checkDone.value = true), CHECK_MS)
})
const extChecking = computed(() => status?.value?.extState === 'never' && !checkDone.value)

// 手动安装引导弹窗（信息条 + 下载 / 前往安装 两步）
const exporting = ref(false)
const showGuide = ref(false)
const exportErr = ref('')

function openGuide() {
  exportErr.value = ''
  showGuide.value = true
}

async function download() {
  exporting.value = true
  exportErr.value = ''
  try {
    const r = await exportExtension()
    if (!(r.ok && r.path)) {
      exportErr.value = r.error ? sanitizeError(r.error) : '下载失败，请重试'
    }
  } catch (e) {
    exportErr.value = sanitizeError(e)
  } finally {
    exporting.value = false
  }
}

// edge:// 等特权页禁止命令行直达，浏览器只能裸启动；
// 地址由前端复制到剪贴板，引导用户粘贴访问（Chrome 用户改用 chrome://extensions）
const EXT_PAGE_URL = 'edge://extensions'

// Edge 商店已上架，常规安装直达商店页（https 可由系统默认浏览器直接打开）；
// 商店不可达或 Chrome 用户走手动弹窗。
const EXT_STORE_URL =
  'https://microsoftedge.microsoft.com/addons/detail/absorb-%E9%98%85%E8%AF%BB%E5%8A%A9%E6%89%8B/nfnkckljlhlcoppcpoiomcfhdgpmdfld'

// 外链用系统浏览器打开（Wails 运行时注入，测试环境回退 window.open）
function openStore() {
  const rt = (window as any).runtime
  if (rt?.BrowserOpenURL) rt.BrowserOpenURL(EXT_STORE_URL)
  else window.open(EXT_STORE_URL, '_blank')
}

const copied = ref(false)

async function copyExtURL() {
  try {
    await navigator.clipboard.writeText(EXT_PAGE_URL)
  } catch {
    const ta = document.createElement('textarea')
    ta.value = EXT_PAGE_URL
    document.body.appendChild(ta)
    ta.select()
    document.execCommand('copy')
    document.body.removeChild(ta)
  }
  copied.value = true
  setTimeout(() => (copied.value = false), 2000)
}

async function gotoExtensions() {
  await openExtensionsPage()
  await copyExtURL()
}
</script>

<template>
  <div>
    <h1 class="mb-4 text-lg font-semibold">状态</h1>

    <div class="grid grid-cols-2 gap-4">
      <!-- 浏览器扩展（四态） -->
      <div class="card flex flex-col">
        <div class="mb-3 text-sm font-medium">浏览器扩展</div>

        <template v-if="status?.extState === 'connected'">
          <div class="flex items-center gap-2">
            <span class="status-dot inline-block h-2.5 w-2.5 rounded-full bg-success" />
            <span class="text-[15px]">已连接</span>
          </div>
          <div class="mt-1 text-xs text-muted">在网页里划词提问即可使用。</div>
        </template>

        <template v-else-if="status?.extState === 'disconnected'">
          <div class="flex items-center gap-2">
            <span class="status-dot inline-block h-2.5 w-2.5 rounded-full bg-muted" />
            <span class="text-[15px]">扩展已断开</span>
          </div>
          <div class="mt-1 text-xs text-muted">浏览器开着，但收不到扩展消息，扩展可能已被卸载。</div>
          <div class="mt-auto flex flex-wrap items-center gap-3 pt-3">
            <button class="btn btn-ghost whitespace-nowrap" @click="openStore">
              重新安装扩展
              <ExternalLink :size="14" />
            </button>
            <button class="text-xs text-muted hover:text-txt hover:underline" @click="openGuide">无法访问商店？尝试手动安装</button>
          </div>
        </template>

        <template v-else-if="status?.extState === 'browser-closed'">
          <div class="flex items-center gap-2">
            <span class="status-dot inline-block h-2.5 w-2.5 rounded-full border border-muted" />
            <span class="text-[15px] text-txt2">浏览器未运行</span>
          </div>
          <div class="mt-1 text-xs text-muted">打开浏览器后，扩展会自动连接到这里。</div>
        </template>

        <template v-else-if="extChecking">
          <div class="flex items-center gap-2">
            <span class="status-dot inline-block h-2.5 w-2.5 animate-pulse rounded-full border border-muted" />
            <span class="text-[15px] text-txt2">检查中…</span>
          </div>
          <div class="mt-auto flex flex-wrap items-center gap-3 pt-3">
            <button class="btn btn-ghost whitespace-nowrap" disabled>检查中…</button>
            <button class="text-xs text-muted hover:text-txt hover:underline" @click="openGuide">无法访问商店？尝试手动安装</button>
          </div>
        </template>

        <template v-else>
          <div class="flex items-center gap-2">
            <span class="status-dot inline-block h-2.5 w-2.5 rounded-full border border-muted" />
            <span class="text-[15px] text-txt2">未连接</span>
          </div>
          <div class="mt-auto flex flex-wrap items-center gap-3 pt-3">
            <button class="btn btn-primary whitespace-nowrap" @click="openStore">
              安装扩展
              <ExternalLink :size="14" />
            </button>
            <button class="text-xs text-muted hover:text-txt hover:underline" @click="openGuide">无法访问商店？尝试手动安装</button>
          </div>
        </template>
      </div>

      <!-- 模型服务 -->
      <div class="card flex flex-col">
        <div class="mb-3 text-sm font-medium">模型服务</div>
        <template v-if="status?.modelName && status?.keyConfigured">
          <div class="flex items-center gap-2">
            <span class="status-dot inline-block h-2.5 w-2.5 rounded-full bg-success" />
            <span class="text-[15px]">已就绪</span>
          </div>
          <div class="mt-1 text-xs text-muted">当前模型：{{ status?.modelName }}</div>
          <div class="mt-auto pt-3">
            <router-link to="/models" class="inline-flex items-center gap-1 text-sm text-primary hover:underline">去管理模型 <ArrowRight :size="14" /></router-link>
          </div>
        </template>
        <template v-else-if="status?.modelName">
          <div class="flex items-center gap-2">
            <span class="status-dot inline-block h-2.5 w-2.5 rounded-full bg-warning" />
            <span class="text-[15px]">密钥未配置</span>
          </div>
          <div class="mt-1 text-xs text-muted">模型「{{ status?.modelName }}」还缺少 API 密钥。</div>
          <div class="mt-auto pt-3">
            <router-link to="/models" class="inline-flex items-center gap-1 text-sm text-primary hover:underline">去编辑 <ArrowRight :size="14" /></router-link>
          </div>
        </template>
        <template v-else>
          <div class="flex items-center gap-2">
            <span class="status-dot inline-block h-2.5 w-2.5 rounded-full bg-warning" />
            <span class="text-[15px]">还没有可用模型</span>
          </div>
          <div class="mt-1 text-xs text-muted">添加并选中一个模型后即可开始回答问题。</div>
          <div class="mt-auto pt-3">
            <router-link to="/models" class="inline-flex items-center gap-1 text-sm text-primary hover:underline">去添加 <ArrowRight :size="14" /></router-link>
          </div>
        </template>
      </div>
    </div>

    <!-- Token 用量（telemetry 第二批）：紧凑概览卡，问答/保存知识后点右上角刷新 -->
    <div v-if="tokenStats" class="card mt-4">
      <div class="mb-2 flex items-center justify-between">
        <div class="text-sm font-medium">Token 用量</div>
        <button class="icon-btn" title="刷新" @click="refreshTokenStats">
          <RefreshCw :size="14" />
        </button>
      </div>
      <div class="flex flex-wrap items-baseline gap-x-4 gap-y-1 text-sm">
        <span>
          今日 <span class="font-semibold">{{ fmtTokens(tokenStats.today.totalTokens) }}</span> tokens
        </span>
        <span class="text-muted">调用 {{ tokenStats.today.calls }} 次</span>
        <span class="text-muted">缓存命中 {{ cacheHitPct }}%</span>
      </div>
      <div class="mt-1 text-xs text-muted">
        累计 {{ fmtTokens(tokenStats.total.totalTokens) }} tokens
      </div>
    </div>

    <!-- 手动安装引导弹窗 -->
    <Transition name="modal">
      <div v-if="showGuide" class="fixed inset-0 z-50 flex items-center justify-center bg-black/40" @click.self="showGuide = false">
        <div class="card modal-card w-[520px]">
          <div class="mb-4 flex items-center justify-between">
            <div class="text-base font-semibold">手动安装扩展</div>
            <button class="icon-btn" title="关闭" @click="showGuide = false"><X :size="16" /></button>
          </div>

        <div class="mb-4 rounded-lg border border-brd bg-primary/5 px-3 py-2.5 text-sm">
          商店无法访问或使用 Chrome 浏览器时，可按以下步骤手动安装。安装前请先在浏览器扩展页启用「开发者模式」。
        </div>

        <div class="space-y-3">
          <div class="flex items-center justify-between rounded-lg border border-brd px-4 py-3">
            <div class="flex gap-3">
              <span class="flex h-6 w-6 shrink-0 items-center justify-center rounded-full bg-primary/10 text-xs font-semibold text-primary">1</span>
              <div>
                <div class="text-sm font-medium">下载扩展</div>
                <div class="mt-0.5 text-xs text-muted">下载「Absorb」扩展，完成后将获得一个扩展文件夹，并自动打开所在位置。</div>
                <div v-if="exportErr" class="mt-1 text-xs text-danger">{{ exportErr }}</div>
              </div>
            </div>
            <button class="btn btn-ghost min-w-[120px] whitespace-nowrap shrink-0" :disabled="exporting" @click="download">
              <Download :size="14" />
              {{ exporting ? '下载中…' : '点击下载' }}
            </button>
          </div>

          <div class="flex items-center justify-between rounded-lg border border-brd px-4 py-3">
            <div class="flex gap-3">
              <span class="flex h-6 w-6 shrink-0 items-center justify-center rounded-full bg-primary/10 text-xs font-semibold text-primary">2</span>
              <div>
                <div class="text-sm font-medium">拖拽安装</div>
                <div class="mt-0.5 text-xs text-muted">
                  点击「打开浏览器」，在地址栏粘贴进入扩展管理页（Chrome 地址为 chrome://extensions），再将「absorb-extension」文件夹拖入页面即可自动安装。
                </div>
                <button
                  class="mt-1.5 inline-flex items-center gap-1 rounded border border-brd px-1.5 py-0.5 font-mono text-xs text-muted hover:text-txt1"
                  @click="copyExtURL"
                >
                  edge://extensions
                  <template v-if="copied"><Check :size="12" /> 已复制</template>
                  <template v-else><Copy :size="12" /> 复制</template>
                </button>
              </div>
            </div>
            <button class="btn btn-ghost min-w-[120px] whitespace-nowrap shrink-0" @click="gotoExtensions">
              <ExternalLink :size="14" />
              打开浏览器
            </button>
          </div>
        </div>

        <div class="mt-4 text-xs text-muted">启用扩展后将自动建立连接，无需额外配置。</div>
        </div>
      </div>
    </Transition>
  </div>
</template>
