<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { ArrowRight, Eye, EyeOff, Pencil, Plus, Trash2 } from 'lucide-vue-next'
import {
  addModelConfig,
  deleteModelConfig,
  listModelConfigs,
  setDefaultModel,
  testModelConnection,
  updateModelConfig,
  type ModelConfigInput,
  type ModelConfigView,
  type ModelTestResult,
} from '../lib/api'
import { PROVIDERS, providerIcon, providerName, type ProviderPreset } from '../lib/providers'
import { sanitizeError } from '../lib/messages'

const list = ref<ModelConfigView[]>([])
const loading = ref(true)

// 仅首次（列表为空时）显示加载中；后续刷新静默换数据，
// 避免三态 v-if 切换把整张列表 DOM 销毁重建导致切换模型时闪烁
async function load() {
  if (list.value.length === 0) loading.value = true
  try {
    list.value = await listModelConfigs()
  } finally {
    loading.value = false
  }
}
onMounted(load)

function displayName(m: ModelConfigView): string {
  return m.displayName || m.modelId
}

// 行点击 = 设为当前使用（单选语义，无启停开关）
async function chooseDefault(m: ModelConfigView) {
  if (m.isDefault) return
  await setDefaultModel(m.id)
  await load()
}

// ---------- 添加/编辑弹窗（两步：选服务商 → 填表单） ----------

const show = ref(false)
const step = ref<1 | 2>(1)
const editing = ref<ModelConfigView | null>(null) // 编辑目标（编辑跳过第一步）

const form = reactive({
  provider: '',
  modelId: '',
  displayName: '', // 展示名，空=显示模型 ID
  apiKey: '',
  baseUrl: '',
  fullUrl: false,
  maxTokens: '', // '' = 不限制（输出上限由模型侧决定）
  thinking: 0, // 0=跟随模型默认 1=开启 2=关闭
  toolRounds: '', // '' = 框架默认
  imageSupport: true,
})
const showAdv = ref(false)
const showKey = ref(false)
const formErr = ref('')
const saving = ref(false)
const testing = ref(false)
const testRes = ref<ModelTestResult | null>(null)

// 输出上限快捷档位（点击直接填入输入框）
const outputChips = [
  { label: '4k', value: '4000' },
  { label: '16k', value: '16000' },
  { label: '32k', value: '32000' },
  { label: '128k', value: '128000' },
]

const preset = computed(() => PROVIDERS.find((p) => p.id === form.provider))
const presetModels = computed(() => preset.value?.models ?? [])
const keyPlaceholder = computed(() =>
  editing.value?.hasKey ? `已保存（尾号 ${editing.value.keyMasked.slice(-4)}），留空保持不变` : '粘贴服务商的 API 密钥',
)

function fillForm(p: ProviderPreset | null) {
  form.provider = p?.id ?? ''
  form.modelId = p?.models[0] ?? ''
  form.displayName = ''
  form.apiKey = ''
  form.baseUrl = p?.baseUrl ?? ''
  form.fullUrl = false
  form.maxTokens = ''
  form.thinking = 0
  form.toolRounds = ''
  form.imageSupport = true
  testRes.value = null
  formErr.value = ''
}

function openAdd() {
  editing.value = null
  fillForm(null)
  step.value = 1
  show.value = true
}

function pick(p: ProviderPreset) {
  editing.value = null
  fillForm(p)
  step.value = 2
}

function openEdit(m: ModelConfigView) {
  editing.value = m
  form.provider = m.provider
  form.modelId = m.modelId
  form.displayName = m.displayName
  form.apiKey = ''
  form.baseUrl = m.baseUrl
  form.fullUrl = m.fullUrl
  form.maxTokens = m.maxTokens == null ? '' : String(m.maxTokens)
  form.thinking = m.thinking
  form.toolRounds = m.toolRounds == null ? '' : String(m.toolRounds)
  form.imageSupport = m.imageSupport
  testRes.value = null
  formErr.value = ''
  step.value = 2
  show.value = true
}

function close() {
  show.value = false
}

// 重置：回到当前模式的初始值
function resetForm() {
  if (editing.value) {
    openEdit(editing.value)
  } else {
    fillForm(preset.value ?? null)
  }
}

// payload 表单 → 绑定输入（上限/轮数空值转 null）。
// 数字输入框的 v-model 会把值 cast 成 number，统一 String 化后再处理。
function payload(): ModelConfigInput {
  const mt = String(form.maxTokens ?? '').trim()
  const tr = String(form.toolRounds ?? '').trim()
  return {
    id: editing.value?.id,
    provider: form.provider,
    displayName: form.displayName.trim(),
    modelId: form.modelId.trim(),
    baseUrl: form.baseUrl.trim(),
    fullUrl: form.fullUrl,
    apiKey: form.apiKey.trim(),
    maxTokens: mt === '' ? null : Math.trunc(Number(mt)),
    thinking: form.thinking,
    toolRounds: tr === '' ? null : Math.trunc(Number(tr)),
    imageSupport: form.imageSupport,
  }
}

function validate(): string {
  if (!form.modelId.trim()) return '请填写模型 ID'
  const base = form.baseUrl.trim()
  if (!base) return '请填写请求地址'
  if (!base.startsWith('http://') && !base.startsWith('https://')) return '请求地址需以 http:// 或 https:// 开头'
  const tr = String(form.toolRounds ?? '').trim()
  if (tr !== '' && Math.trunc(Number(tr)) < 1) return '工具调用轮数需为正整数，留空使用默认'
  return ''
}

async function doTest() {
  const err = validate()
  if (err) {
    formErr.value = err
    return
  }
  formErr.value = ''
  testing.value = true
  try {
    testRes.value = await testModelConnection(payload())
  } catch (e) {
    testRes.value = { ok: false, latencyMs: 0, error: sanitizeError(e) }
  } finally {
    testing.value = false
  }
}

async function submit() {
  const err = validate()
  if (err) {
    formErr.value = err
    return
  }
  if (saving.value) return
  saving.value = true
  formErr.value = ''
  try {
    if (editing.value) {
      await updateModelConfig(payload())
    } else {
      await addModelConfig(payload())
    }
    close()
    await load()
  } catch (e) {
    formErr.value = sanitizeError(e)
  } finally {
    saving.value = false
  }
}

// 外链用系统浏览器打开（Wails 运行时注入，测试环境回退 window.open）
function openExternal(u: string) {
  const rt = (window as any).runtime
  if (rt?.BrowserOpenURL) rt.BrowserOpenURL(u)
  else window.open(u, '_blank')
}

// ---------- 删除 ----------

const confirmId = ref('')

function askDelete(m: ModelConfigView) {
  confirmId.value = m.id
}

async function doDelete() {
  const id = confirmId.value
  confirmId.value = ''
  if (!id) return
  await deleteModelConfig(id)
  await load()
}

const confirmMeta = computed(() => list.value.find((m) => m.id === confirmId.value))
</script>

<template>
  <div>
    <div class="mb-5 flex items-baseline justify-between">
      <h1 class="text-lg font-semibold">模型</h1>
      <button class="btn btn-primary" @click="openAdd">
        <Plus :size="14" />
        <span>添加模型</span>
      </button>
    </div>
    <p class="mb-4 text-sm text-muted">配置 API Key 添加更多可用模型，提问使用当前选中的模型。</p>

    <div v-if="loading" class="text-sm text-muted">加载中…</div>
    <div v-else-if="list.length === 0" class="card text-sm text-muted">
      还没有可用模型。点击"添加模型"接入第一个模型。
    </div>
    <!-- 模型表格 -->
    <div v-else class="card overflow-hidden !p-0">
      <table class="w-full text-sm">
        <thead>
          <tr class="border-b border-brd text-left text-xs text-muted">
            <th class="px-5 py-2.5 font-medium">模型</th>
            <th class="px-5 py-2.5 font-medium">服务商</th>
            <th class="px-5 py-2.5 font-medium">密钥</th>
            <th class="px-3 py-2.5 text-right font-medium">操作</th>
          </tr>
        </thead>
        <tbody>
          <tr
            v-for="m in list"
            :key="m.id"
            class="table-row cursor-pointer border-b border-brd last:border-b-0 hover:bg-primary-soft"
            :title="m.isDefault ? '' : '点击设为当前使用'"
            @click="chooseDefault(m)"
          >
            <td class="px-5 py-3">
              <div class="flex items-center gap-2.5">
                <span class="inline-block h-2.5 w-2.5 shrink-0 rounded-full" :class="m.isDefault ? 'bg-success' : 'border border-muted'" />
                <div class="min-w-0">
                  <div class="truncate font-medium">
                    {{ displayName(m) }}
                    <span v-if="m.isDefault" class="ml-1 align-middle text-xs text-success">当前使用</span>
                  </div>
                  <!-- 方案 A：副标题只留模型 ID，与标题重复时（未填展示名）隐藏 -->
                  <div v-if="m.displayName" class="truncate text-xs text-muted">{{ m.modelId }}</div>
                </div>
              </div>
            </td>
            <td class="px-5 py-3 text-muted">
              <span class="inline-flex items-center gap-1.5">
                <img v-if="providerIcon(m.provider)" :src="providerIcon(m.provider)" alt="" class="h-4 w-4 object-contain" />
                {{ providerName(m.provider) }}
              </span>
            </td>
            <td class="px-5 py-3">
              <span :class="m.hasKey ? 'text-success' : 'text-warning'">{{ m.hasKey ? '已配置' : '未配置' }}</span>
            </td>
            <td class="px-3 py-3 text-right">
              <div class="inline-flex gap-1">
                <button class="icon-btn" title="编辑" @click.stop="openEdit(m)">
                  <Pencil :size="15" />
                </button>
                <button class="icon-btn hover:text-danger" title="删除" @click.stop="askDelete(m)">
                  <Trash2 :size="15" />
                </button>
              </div>
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <!-- 添加/编辑弹窗 -->
    <Transition name="modal">
      <div v-if="show" class="fixed inset-0 z-50 flex items-center justify-center bg-black/40" @click.self="close">
        <div class="card modal-card max-h-[85vh] w-[560px] overflow-y-auto">
        <!-- 第一步：服务商网格 -->
        <template v-if="step === 1">
          <div class="mb-3 flex items-baseline justify-between">
            <div class="text-sm font-medium">选择服务商</div>
            <button class="text-xs text-muted hover:text-txt" @click="close">取消</button>
          </div>
          <div class="grid grid-cols-3 gap-2">
            <button
              v-for="p in PROVIDERS"
              :key="p.id"
              class="brd-fade flex items-center gap-2 rounded-lg border border-brd px-3 py-2.5 text-left hover:border-primary"
              @click="pick(p)"
            >
              <span v-if="p.icon" class="flex h-7 w-7 shrink-0 items-center justify-center">
                <img :src="p.icon" :alt="p.name" class="h-6 w-6 object-contain" />
              </span>
              <span v-else class="flex h-7 w-7 shrink-0 items-center justify-center rounded-md bg-primary/10 text-xs font-semibold text-primary">
                {{ p.logo }}
              </span>
              <span class="truncate text-sm">{{ p.name }}</span>
            </button>
          </div>
        </template>

        <!-- 第二步：参数表单 -->
        <template v-else>
          <div class="mb-4 flex items-baseline justify-between">
            <div class="text-sm font-medium">{{ editing ? '编辑模型' : '添加模型' }}</div>
            <button class="text-xs text-muted hover:text-txt" @click="close">取消</button>
          </div>

          <div class="flex flex-col gap-4">
            <label class="block">
              <span class="mb-1 block text-sm">服务商</span>
              <input class="input bg-surface2" :value="preset?.name ?? form.provider" readonly />
            </label>

            <label class="block">
              <span class="mb-1 block text-sm">模型 ID</span>
              <input v-model.trim="form.modelId" class="input" list="model-id-options" placeholder="输入或选择模型 ID" />
              <datalist id="model-id-options">
                <option v-for="m in presetModels" :key="m" :value="m" />
              </datalist>
            </label>

            <label class="block">
              <span class="mb-1 block text-sm">展示名</span>
              <input v-model.trim="form.displayName" class="input" placeholder="如 GLM-4.6 主力" />
            </label>

            <label class="block">
              <span class="mb-1 block text-sm">API 密钥</span>
              <span class="relative block">
                <input
                  v-model.trim="form.apiKey"
                  class="input pr-9"
                  :type="showKey ? 'text' : 'password'"
                  :placeholder="keyPlaceholder"
                  autocomplete="off"
                />
                <button
                  class="absolute right-2 top-1/2 -translate-y-1/2 text-muted hover:text-txt"
                  :title="showKey ? '隐藏' : '显示'"
                  type="button"
                  @click="showKey = !showKey"
                >
                  <EyeOff v-if="showKey" :size="15" />
                  <Eye v-else :size="15" />
                </button>
              </span>
              <span v-if="preset?.keyUrl" class="mt-1 block text-xs">
                <a href="#" class="inline-flex items-center gap-1 text-primary hover:underline" @click.prevent="openExternal(preset.keyUrl)">获取 API 密钥 <ArrowRight :size="14" /></a>
              </span>
            </label>

            <!-- 高级配置折叠 -->
            <div>
              <button class="text-sm text-txt2 hover:text-txt" type="button" @click="showAdv = !showAdv">
                高级配置 {{ showAdv ? '▴' : '▾' }}
              </button>
              <div v-show="showAdv" class="mt-3 flex flex-col gap-4">
                <label class="block">
                  <span class="mb-1 block text-sm">请求地址</span>
                  <input v-model.trim="form.baseUrl" class="input" placeholder="https://…" />
                  <label class="mt-2 flex items-center gap-2">
                    <input v-model="form.fullUrl" type="checkbox" class="size-4 accent-primary" />
                    <span class="text-xs text-muted">完整 URL（地址已含 /chat/completions 时不自动拼接）</span>
                  </label>
                </label>
                <div>
                  <span class="mb-1 block text-sm">输出 <span class="text-xs text-muted">（Token，留空则由模型侧决定）</span></span>
                  <div class="flex items-center gap-3">
                    <input v-model.trim="form.maxTokens" class="input flex-1" type="number" min="1" placeholder="请输入数值，留空则使用最佳默认值" />
                    <div class="flex shrink-0 gap-2">
                      <button
                        v-for="c in outputChips"
                        :key="c.value"
                        class="text-xs text-muted underline-offset-2 hover:text-primary hover:underline"
                        type="button"
                        @click="form.maxTokens = c.value"
                      >
                        {{ c.label }}
                      </button>
                    </div>
                  </div>
                </div>
                <label class="block">
                  <span class="mb-1 block text-sm">工具调用轮数 <span class="text-xs text-muted">（划词问答调用页面工具的最大轮数）</span></span>
                  <input v-model.trim="form.toolRounds" class="input" type="number" min="1" placeholder="请输入数值，留空则使用默认值" />
                </label>
                <div>
                  <span class="mb-1 block text-sm">支持图片输入</span>
                  <div class="flex gap-6">
                    <label class="flex cursor-pointer items-center gap-1.5 text-sm">
                      <input v-model="form.imageSupport" type="radio" name="img-support" :value="true" class="size-4 accent-primary" />
                      支持
                    </label>
                    <label class="flex cursor-pointer items-center gap-1.5 text-sm">
                      <input v-model="form.imageSupport" type="radio" name="img-support" :value="false" class="size-4 accent-primary" />
                      不支持
                    </label>
                  </div>
                </div>
                <div>
                  <span class="mb-1 block text-sm">思考模式</span>
                  <div class="flex gap-6">
                    <label class="flex cursor-pointer items-center gap-1.5 text-sm">
                      <input v-model.number="form.thinking" type="radio" name="thinking" :value="0" class="size-4 accent-primary" />
                      跟随模型默认配置
                    </label>
                    <label class="flex cursor-pointer items-center gap-1.5 text-sm">
                      <input v-model.number="form.thinking" type="radio" name="thinking" :value="1" class="size-4 accent-primary" />
                      开启
                    </label>
                    <label class="flex cursor-pointer items-center gap-1.5 text-sm">
                      <input v-model.number="form.thinking" type="radio" name="thinking" :value="2" class="size-4 accent-primary" />
                      关闭
                    </label>
                  </div>
                </div>
              </div>
            </div>

            <p v-if="formErr" class="text-sm text-danger">{{ formErr }}</p>
            <p v-if="testRes && testRes.ok" class="text-sm text-success">连接成功 · {{ testRes.latencyMs }}ms</p>
            <p v-else-if="testRes && !testRes.ok" class="text-sm text-danger">{{ testRes.error }}</p>
          </div>

          <div class="mt-5 border-t border-brd pt-4">
            <p class="mb-3 text-xs text-muted">连通性测试会发起一次真实请求，会消耗少量模型 Token。</p>
            <div class="flex justify-end gap-2">
              <button class="btn btn-ghost" :disabled="saving || testing" @click="resetForm">重置</button>
              <button class="btn btn-ghost" :disabled="saving || testing" @click="doTest">
                {{ testing ? '测试中…' : '测试' }}
              </button>
              <button class="btn btn-primary" :disabled="saving || testing" @click="submit">
                {{ saving ? '保存中…' : editing ? '保存' : '添加' }}
              </button>
            </div>
          </div>
        </template>
        </div>
      </div>
    </Transition>

    <!-- 删除确认层 -->
    <Transition name="modal">
      <div v-if="confirmId" class="fixed inset-0 z-50 flex items-center justify-center bg-black/40" @click.self="confirmId = ''">
        <div class="card modal-card w-[360px]">
          <div class="mb-2 text-sm font-medium">删除这个模型？</div>
          <div class="mb-4 text-xs text-muted">「{{ confirmMeta ? displayName(confirmMeta) : '' }}」删除后无法恢复。</div>
          <div class="flex justify-end gap-2">
            <button class="btn btn-ghost" @click="confirmId = ''">取消</button>
            <button class="btn btn-danger" @click="doDelete">删除</button>
          </div>
        </div>
      </div>
    </Transition>
  </div>
</template>
