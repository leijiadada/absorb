// 用户可见文案集中地：错误 code → 友好文本，外加通用原始错误消毒。
//
// 后端（internal/upgrade）把用户可见错误统一成带 [CODE] 前缀的字符串跨 Wails
// 边界传出；本文件是「code → 中文友好文案」的唯一映射处。后续要做 i18n 时，只需
// 把 UPDATE_MESSAGES 换成 { zh_CN, en } 字典即可，调用点无需改动（即 i18n-ready）。

export type Level = 'info' | 'error'

export interface Friendly {
  level: Level // info=灰字提示；error=红字
  title: string
  detail: string
}

// 升级错误 code（来自后端 Error().Error() 的 [CODE] 前缀）→ 友好文案。
export const UPDATE_MESSAGES: Record<string, Friendly> = {
  feed_unavailable: { level: 'error', title: '更新源暂时无法访问', detail: '请稍后重试，或前往项目主页查看是否有新版本。' },
  net_fail: { level: 'error', title: '网络连接失败', detail: '请检查网络后重试。' },
  bad_feed: { level: 'error', title: '更新信息格式异常', detail: '发布源数据有误，请稍后重试或联系开发者。' },
  no_feed_url: { level: 'error', title: '未配置更新源', detail: '当前环境未配置更新检查地址，无法在线检查更新。' },
  no_artifact: { level: 'error', title: '当前平台暂不支持自动更新', detail: '请前往项目主页手动下载对应安装包。' },
  checksum: { level: 'error', title: '安装包校验失败', detail: '下载的文件不完整或被篡改，已取消更新，请重新检查更新。' },
  insecure_url: { level: 'error', title: '更新源地址不安全', detail: '更新源必须使用 https，已拒绝检查。' },
  unsupported: { level: 'error', title: '当前系统不支持自动更新', detail: '请手动下载安装包升级。' },
  backup_fail: { level: 'error', title: '升级前备份失败', detail: '为保证数据安全已中止更新，请查看日志后重试。' },
  download_fail: { level: 'error', title: '下载更新失败', detail: '请检查网络后重试；若反复失败请手动下载安装包。' },
  install_fail: { level: 'error', title: '安装失败', detail: '请手动运行安装包完成升级，或查看日志。' },
  not_enabled: { level: 'error', title: '升级功能未启用', detail: '请检查应用配置后重试。' },
  no_pending: { level: 'error', title: '暂无可安装的更新', detail: '请先点击「检查更新」发现新版本。' },
  dev_build: { level: 'info', title: '开发版本', detail: '开发构建不参与在线更新检查。' },
  disabled: { level: 'info', title: '', detail: '自动更新检查已关闭。' },
  throttled: { level: 'info', title: '', detail: '检查过于频繁，稍后再试。' },
  no_update: { level: 'info', title: '', detail: '已是最新版本。' },
  unknown: { level: 'error', title: '操作失败', detail: '请稍后重试；若持续出现请查看控制台日志。' },
}

const FALLBACK: Friendly = UPDATE_MESSAGES.unknown

const CODE_RE = /^\[([a-z0-9_]+)\]/

// 从后端错误串里取出 [CODE]；无前缀返回 null。
export function parseCode(s: string): string | null {
  const m = CODE_RE.exec((s ?? '').trim())
  return m ? m[1] : null
}

// 把后端升级错误（可能带 [CODE] 前缀）转成友好结构；原始错误串仅进 console，不泄露给用户。
export function mapUpdateError(err: unknown): Friendly {
  const raw = err instanceof Error ? err.message : String(err ?? '')
  const code = parseCode(raw)
  if (code && UPDATE_MESSAGES[code]) {
    console.warn('[update]', raw)
    return UPDATE_MESSAGES[code]
  }
  console.warn('[update] unhandled error:', raw)
  return FALLBACK
}

// 后端经 view.message 透出的结论：带 code 则转友好；否则按普通提示原样返回（已是最新版本等）。
export function mapUpdateMessage(s: string): Friendly | null {
  const code = parseCode(s)
  if (code && UPDATE_MESSAGES[code]) return UPDATE_MESSAGES[code]
  return null
}

// 通用消毒：把任意后端原始错误尽量说成人话，绝不把英文/堆栈/状态码透传给用户。
export function sanitizeError(err: unknown): string {
  const raw = err instanceof Error ? err.message : String(err ?? '')
  const lower = raw.toLowerCase()
  if (/connection|econnrefused|timeout|deadline|dns|network|socket|tls|x509|certificate/.test(lower)) {
    return '网络连接失败，请检查网络或代理设置后重试。'
  }
  if (/401|unauthorized|forbidden|permission/.test(lower)) {
    return '鉴权失败：密钥可能无效或已过期，请检查配置。'
  }
  if (/404|not found/.test(lower)) {
    return '请求的资源不存在，请检查地址是否正确。'
  }
  if (/400|bad request/.test(lower)) {
    return '请求参数有误，请检查配置后重试。'
  }
  if (/\b5\d\d\b/.test(raw)) {
    return '服务暂时不可用，请稍后重试。'
  }
  return '操作失败，请稍后重试；若持续出现请查看控制台日志。'
}
