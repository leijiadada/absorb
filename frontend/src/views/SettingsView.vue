<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { ChevronDown, Download, ExternalLink, RefreshCw } from 'lucide-vue-next'
import { getSettings, saveSettings, type Settings } from '../lib/api'
import { useUpdate } from '../lib/update'

const form = ref<Settings>({
  memoryEnabled: true,
  proxyUrl: '',
  systemProxy: '',
  proxyOverride: '',
  configPath: '',
  dataRoot: '',
  version: '',
})

// 代理模式为前端派生态：proxyUrl 非空即手动、空即跟随系统，不落盘独立字段
const proxyMode = ref<'system' | 'manual'>('system')

const loading = ref(true)
const saving = ref(false)
const saved = ref(false)

const { view, checking, applying, progress, error, statusMessage, check, apply, initUpdate, openExternal } =
  useUpdate()

// 例外地址以分号分隔：摘要行只显示项数，点击展开完整列表
const overrideList = computed(
  () => form.value.proxyOverride.split(';').map((s) => s.trim()).filter(Boolean),
)
const showOverride = ref(false)

// 版本号截去括号里的构建时间，与底栏口径一致
const versionText = computed(() =>
  (view.current || form.value.version || '—').split(' (')[0],
)

onMounted(async () => {
  try {
    form.value = await getSettings()
    proxyMode.value = form.value.proxyUrl ? 'manual' : 'system'
  } finally {
    loading.value = false
  }
  initUpdate() // 订阅事件 + 拉取最近检查结论（幂等）
})

async function save() {
  if (saving.value) return
  saving.value = true
  try {
    await saveSettings({
      ...form.value,
      proxyUrl: proxyMode.value === 'manual' ? form.value.proxyUrl : '',
    })
    saved.value = true
    window.setTimeout(() => (saved.value = false), 4000)
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <div>
    <h1 class="mb-4 text-lg font-semibold">设置</h1>

    <div v-if="loading" class="text-sm text-muted">加载中…</div>

    <div v-else class="flex flex-col gap-4">
      <!-- 记忆：行式，控件右对齐 -->
      <div class="card flex items-center justify-between gap-4">
        <div>
          <div class="text-sm font-medium">长期记忆</div>
          <p class="mt-0.5 text-xs text-muted">记住你的背景与偏好，让回答更贴合你。</p>
        </div>
        <input v-model="form.memoryEnabled" type="checkbox" class="size-4 shrink-0 accent-primary" />
      </div>

      <!-- 网络代理：标题行右 select，状态折叠为摘要行，保存随卡 -->
      <div class="card">
        <div class="flex items-center justify-between gap-4">
          <div>
            <div class="text-sm font-medium">网络代理</div>
            <p class="mt-0.5 text-xs text-muted">自动跟随系统代理设置</p>
          </div>
          <!-- .input 是 width:100%（无 layer CSS，优先级高于 Tailwind w-32），定宽靠外层容器 -->
          <div class="w-32 shrink-0">
            <select v-model="proxyMode" class="input">
              <option value="system">跟随系统</option>
              <option value="manual">手动</option>
            </select>
          </div>
        </div>

        <!-- 跟随系统：一行摘要（状态点 + 地址 + 例外项数） -->
        <div
          v-if="proxyMode === 'system'"
          class="mt-4 flex items-center gap-2 border-t border-brd pt-3 text-xs"
        >
          <template v-if="form.systemProxy">
            <span class="size-1.5 shrink-0 rounded-full bg-success" />
            <span>系统代理 {{ form.systemProxy }}</span>
            <button
              v-if="overrideList.length"
              class="inline-flex items-center gap-1 rounded text-muted transition-colors hover:text-txt2"
              @click="showOverride = !showOverride"
            >
              · 例外地址 {{ overrideList.length }} 项
              <ChevronDown
                :size="12"
                class="transition-transform duration-150"
                :class="showOverride ? 'rotate-180' : ''"
              />
            </button>
          </template>
          <span v-else class="text-muted">未检测到系统代理，将直连</span>
        </div>

        <!-- 例外地址完整列表（点击摘要行展开） -->
        <div v-if="showOverride && overrideList.length" class="mt-2 rounded-lg border border-brd bg-brd/30 px-3 py-2.5">
          <div class="flex flex-wrap gap-1.5">
            <span
              v-for="item in overrideList"
              :key="item"
              class="rounded border border-brd bg-surface px-1.5 py-0.5 font-mono text-[11px] text-txt2"
            >{{ item }}</span>
          </div>
        </div>

        <!-- 手动：固定代理地址输入 -->
        <div v-else class="mt-4 border-t border-brd pt-3">
          <input v-model.trim="form.proxyUrl" class="input" placeholder="例如 http://127.0.0.1:7897" />
          <p class="mt-1 text-xs text-muted">填写固定代理地址；切回"跟随系统"并保存即清除。</p>
        </div>

        <!-- 保存收进作用对象（grouping：按钮贴近它影响的东西） -->
        <div class="mt-4 flex items-center justify-end gap-3">
          <span v-if="saved" class="text-xs text-success">已保存，改动即时生效。</span>
          <button class="btn btn-primary" :disabled="saving" @click="save">
            {{ saving ? '保存中…' : '保存' }}
          </button>
        </div>
      </div>

      <!-- 更新：标题行右检查更新，新版本改为内联通知条（去卡中卡） -->
      <div class="card">
        <div class="flex items-center justify-between gap-4">
          <div>
            <div class="text-sm font-medium">更新</div>
            <p class="mt-0.5 text-xs text-muted">当前版本 {{ versionText }}</p>
          </div>
          <button class="btn btn-ghost shrink-0" :disabled="checking" @click="check">
            <RefreshCw v-if="!checking" :size="14" class="mr-1" />
            {{ checking ? '检查中…' : '检查更新' }}
          </button>
        </div>

        <div v-if="error" class="mt-2 text-xs text-danger">
          {{ error.title }}<template v-if="error.detail">：{{ error.detail }}</template>
        </div>
        <div v-else-if="checking" class="mt-2 text-xs text-muted">正在检查更新…</div>
        <div
          v-else-if="statusMessage"
          class="mt-2 text-xs"
          :class="statusMessage.level === 'error' ? 'text-danger' : 'text-muted'"
        >
          {{ statusMessage.detail || statusMessage.title }}
        </div>

        <!-- 发现新版本：浅蓝通知条 = 文案 + 主按钮 -->
        <div
          v-if="view.available"
          class="mt-4 flex items-center justify-between gap-4 rounded-lg bg-primary-soft p-3"
        >
          <div class="min-w-0">
            <div class="flex items-center gap-2 text-sm font-medium">
              <span>发现新版本 v{{ view.latest }}</span>
              <span
                v-if="view.mandatory"
                class="rounded bg-danger/10 px-1.5 py-0.5 text-xs text-danger"
                >必须更新</span
              >
            </div>
            <button
              v-if="view.notesUrl"
              class="mt-0.5 inline-flex items-center gap-1 text-xs text-primary hover:underline"
              @click="openExternal(view.notesUrl)"
            >
              <ExternalLink :size="12" />查看发布说明
            </button>
          </div>
          <button class="btn btn-primary shrink-0" :disabled="applying" @click="apply">
            <Download :size="14" class="mr-1" />
            {{ applying ? '正在更新…' : '下载并更新' }}
          </button>
        </div>

        <div v-if="applying && progress !== null" class="mt-3">
          <div class="h-1.5 w-full overflow-hidden rounded bg-brd">
            <!-- scaleX 合成属性驱动，避免 width 动画每帧 layout（review 标准 7） -->
            <div
              class="progress-bar h-full bg-primary"
              :style="{ transform: `scaleX(${progress / 100})` }"
            />
          </div>
          <div class="mt-1 text-xs text-muted">下载中 {{ progress }}%</div>
        </div>
      </div>
    </div>
  </div>
</template>
