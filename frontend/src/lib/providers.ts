// 服务商预置：添加模型弹窗第一层网格与表单预填。
// 全部走 OpenAI 兼容协议；models 为常用模型 ID 预置，可手输其他。
// 模型清单核对自各服务商官方文档（2026-11），仅列当前在售主力模型；
// 服务商迭代快，过期条目由用户手输兜底，不做校验源。
import bailianIcon from '../assets/providers/bailian-color.png'
import deepseekIcon from '../assets/providers/deepseek-color.png'
import hunyuanIcon from '../assets/providers/hunyuan-color.png'
import kimiIcon from '../assets/providers/kimi-color.png'
import minimaxIcon from '../assets/providers/minimax-color.png'
import openrouterIcon from '../assets/providers/openrouter-color.png'
import siliconcloudIcon from '../assets/providers/siliconcloud-color.png'
import volcengineIcon from '../assets/providers/volcengine-color.png'
import zhipuIcon from '../assets/providers/zhipu-color.png'

export interface ProviderPreset {
  id: string
  name: string
  logo: string // 徽标文字（首字/简称），无 icon 图时网格回退展示
  icon?: string // 服务商 logo 图片（vite 静态资源 URL），空=仅文字徽标
  baseUrl: string
  models: string[]
  keyUrl: string // 获取 API 密钥的官网链接，空=不展示
}

export const PROVIDERS: ProviderPreset[] = [
  {
    id: 'bigmodel',
    name: 'Bigmodel 智谱',
    logo: '智',
    icon: zhipuIcon,
    baseUrl: 'https://open.bigmodel.cn/api/paas/v4',
    models: ['glm-5.3', 'glm-5.3-flash', 'glm-5.2', 'glm-4.5-flash'],
    keyUrl: 'https://open.bigmodel.cn/usercenter/apikeys',
  },
  {
    id: 'deepseek',
    name: 'DeepSeek',
    logo: 'DS',
    icon: deepseekIcon,
    baseUrl: 'https://api.deepseek.com',
    // deepseek-flash = DeepSeek-V4.1-Flash（官方当前主模型）；deepseek-chat/reasoner 已退役
    models: ['deepseek-flash', 'deepseek-v4-pro'],
    keyUrl: 'https://platform.deepseek.com/api_keys',
  },
  {
    id: 'kimi',
    name: 'Kimi 月之暗面',
    logo: 'K',
    icon: kimiIcon,
    baseUrl: 'https://api.moonshot.cn/v1',
    // kimi-k2 系列已下线（官方公告 2026-05），当前主推 K2.6 / K2.5
    models: ['kimi-k2.6', 'kimi-k2.5', 'moonshot-v1-8k'],
    keyUrl: 'https://platform.moonshot.cn/console/api-keys',
  },
  {
    id: 'dashscope',
    name: '阿里云百炼',
    logo: '阿',
    icon: bailianIcon,
    baseUrl: 'https://dashscope.aliyuncs.com/compatible-mode/v1',
    models: ['qwen3.8-max', 'qwen3.7-plus', 'qwen3.8-flash'],
    keyUrl: 'https://bailian.console.aliyun.com/?apiKey=1',
  },
  {
    id: 'siliconflow',
    name: '硅基流动',
    logo: '硅',
    icon: siliconcloudIcon,
    baseUrl: 'https://api.siliconflow.cn/v1',
    // 硅基流动模型 ID 带组织前缀（模型广场命名规则）
    models: ['deepseek-ai/DeepSeek-V4.1-Flash', 'zai-org/GLM-5.3'],
    keyUrl: 'https://cloud.siliconflow.cn/account/ak',
  },
  {
    id: 'volces',
    name: '火山引擎',
    logo: '火',
    icon: volcengineIcon,
    baseUrl: 'https://ark.cn-beijing.volces.com/api/v3',
    models: [],
    keyUrl: 'https://console.volcengine.com/ark',
  },
  {
    id: 'hunyuan',
    name: '腾讯混元',
    logo: '混',
    icon: hunyuanIcon,
    baseUrl: 'https://api.hunyuan.cloud.tencent.com/v1',
    // hunyuan-turbos 系列已下线，官方迁移至 hy3-preview
    models: ['hy3-preview', 'hunyuan-large'],
    keyUrl: 'https://console.cloud.tencent.com/hunyuan/api-key',
  },
  {
    id: 'minimax',
    name: 'MiniMax',
    logo: 'M',
    icon: minimaxIcon,
    baseUrl: 'https://api.minimaxi.com/v1',
    models: ['MiniMax-M3', 'MiniMax-M2.7'],
    keyUrl: 'https://platform.minimaxi.com/user-center/basic-information/interface-key',
  },
  {
    id: 'openrouter',
    name: 'OpenRouter',
    logo: 'OR',
    icon: openrouterIcon,
    baseUrl: 'https://openrouter.ai/api/v1',
    models: [],
    keyUrl: 'https://openrouter.ai/settings/keys',
  },
  {
    id: 'custom',
    name: '自定义模型',
    logo: '自',
    baseUrl: '',
    models: [],
    keyUrl: '',
  },
]

// providerName 服务商 ID → 显示名（未知 ID 原样返回，兼容旧数据）。
export function providerName(id: string): string {
  return PROVIDERS.find((p) => p.id === id)?.name ?? id
}

// providerIcon 服务商 ID → logo 图片 URL（未知/无图返回 undefined）。
export function providerIcon(id: string): string | undefined {
  return PROVIDERS.find((p) => p.id === id)?.icon
}
