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

// 入口中文名（kind ∈ chat/draft/summary）
const kindLabels: Record<string, string> = { chat: '问答', draft: '草稿', summary: '摘要' }

// 各入口分布（全期）：只列出有调用记录的入口
const kindParts = computed(() =>
  Object.entries(tokenStats.value?.byKind ?? {})
    .filter(([, v]) => v.calls > 0)
    .map(([k, v]) => `${kindLabels[k] ?? k} ${fmtTokens(v.totalTokens)}`),
)

// 相对时间（extLastSeen 为 Unix 秒，0=从未心跳）
function relTime(sec: number): string {
  if (!sec) return '从未'
  const diff = Math.floor(Date.now() / 1000) - sec
  if (diff < 60) return '刚刚'
  if (diff < 3600) return `${Math.floor(diff / 60)} 分钟前`
  if (diff < 86400) return `${Math.floor(diff / 3600)} 小时前`
  return `${Math.floor(diff / 86400)} 天前`
}

// 英雄横幅：综合服务/扩展/模型三源健康度（方案 A 的页面锚点）
// 优先级：服务停止(红) > 部分待就绪(琥珀) > 一切正常(蓝底绿点)
const hero = computed(() => {
  const s = status?.value
  if (!s) return null
  const up = s.uptime || '刚刚启动'
  if (!s.serviceRunning) {
    return {
      cls: 'bg-danger/10',
      dot: 'bg-danger',
      tCls: 'text-danger',
      t: '服务未运行',
      sub: '后台服务已停止，功能暂不可用，可尝试重启应用。',
    }
  }
  const extMap: Record<string, string> = {
    connected: `扩展已连接 · ${relTime(s.extLastSeen)}活跃`,
    disconnected: `扩展已断开 · ${relTime(s.extLastSeen)}活跃`,
    'browser-closed': '浏览器未运行',
    never: '扩展从未连接',
  }
  const modelOk = !!(s.modelName && s.keyConfigured)
  if (s.extState === 'connected' && modelOk) {
    return {
      cls: 'bg-primary-soft',
      dot: 'bg-success',
      tCls: 'text-primary',
      t: '一切正常',
      // 结论层只留一句证据；扩展/模型详情卡片里都有，不重复
      sub: `服务运行 ${up}`,
    }
  }
  // 异常态：横幅只列异常项，负责定位问题；正常项不重复
  const issues: string[] = []
  if (s.extState !== 'connected') issues.push(extMap[s.extState] ?? '扩展未连接')
  if (!modelOk) issues.push('模型未就绪')
  return {
    cls: 'bg-warning/10',
    dot: 'bg-warning',
    tCls: 'text-warning',
    t: '服务运行中',
    sub: [`服务运行 ${up}`, ...issues].join(' · '),
  }
})

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

    <!-- 英雄横幅：第一眼传达整体健康度（apple-design：最重要的东西最明显） -->
    <div v-if="hero" class="mb-4 flex items-center gap-3 rounded-xl px-5 py-3.5" :class="hero.cls">
      <span class="status-dot inline-block h-3 w-3 shrink-0 rounded-full" :class="hero.dot" />
      <div class="min-w-0">
        <div class="text-base font-bold" :class="hero.tCls">{{ hero.t }}</div>
        <div class="mt-0.5 truncate text-xs text-txt2">{{ hero.sub }}</div>
      </div>
    </div>

    <div class="grid grid-cols-2 gap-4">
      <!-- 浏览器扩展（四态） -->
      <div class="card">
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
          <button class="btn btn-ghost mt-2" @click="openGuide">重新安装扩展</button>
        </template>

        <template v-else-if="status?.extState === 'browser-closed'">
          <div class="flex items-center gap-2">
            <span class="status-dot inline-block h-2.5 w-2.5 rounded-full border border-muted" />
            <span class="text-[15px] text-txt2">浏览器未运行</span>
          </div>
          <div class="mt-1 text-xs text-muted">打开浏览器后，扩展会自动连接到这里。</div>
        </template>

        <template v-else>
          <div class="flex items-center gap-2">
            <span class="status-dot inline-block h-2.5 w-2.5 rounded-full border border-muted" />
            <span class="text-[15px] text-txt2">未连接</span>
          </div>
          <div class="mt-1 text-xs text-muted">如果未安装浏览器扩展，划词阅读功能无法使用。</div>
          <button class="btn btn-primary mt-2" @click="openGuide">安装扩展</button>
        </template>
      </div>

      <!-- 模型服务 -->
      <div class="card">
        <div class="mb-3 text-sm font-medium">模型服务</div>
        <template v-if="status?.modelName && status?.keyConfigured">
          <div class="flex items-center gap-2">
            <span class="status-dot inline-block h-2.5 w-2.5 rounded-full bg-success" />
            <span class="text-[15px]">已就绪</span>
          </div>
          <div class="mt-1 text-xs text-muted">当前模型：{{ status?.modelName }}</div>
          <router-link to="/models" class="mt-2 inline-flex items-center gap-1 text-sm text-primary hover:underline">去管理模型 <ArrowRight :size="14" /></router-link>
        </template>
        <template v-else-if="status?.modelName">
          <div class="flex items-center gap-2">
            <span class="status-dot inline-block h-2.5 w-2.5 rounded-full bg-warning" />
            <span class="text-[15px]">密钥未配置</span>
          </div>
          <div class="mt-1 text-xs text-muted">模型「{{ status?.modelName }}」还缺少 API 密钥。</div>
          <router-link to="/models" class="mt-2 inline-flex items-center gap-1 text-sm text-primary hover:underline">去编辑 <ArrowRight :size="14" /></router-link>
        </template>
        <template v-else>
          <div class="flex items-center gap-2">
            <span class="status-dot inline-block h-2.5 w-2.5 rounded-full bg-warning" />
            <span class="text-[15px]">还没有可用模型</span>
          </div>
          <div class="mt-1 text-xs text-muted">添加并选中一个模型后即可开始回答问题。</div>
          <router-link to="/models" class="mt-2 inline-flex items-center gap-1 text-sm text-primary hover:underline">去添加 <ArrowRight :size="14" /></router-link>
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
        <template v-if="kindParts.length"> · {{ kindParts.join(' · ') }}</template>
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
          为顺利添加扩展，请先打开浏览器扩展页并启用「开发者模式」。
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
