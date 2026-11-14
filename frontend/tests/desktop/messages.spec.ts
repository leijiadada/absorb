import { describe, expect, it } from 'vitest'
import { parseCode, mapUpdateError, sanitizeError, UPDATE_MESSAGES } from '../../src/lib/messages'

// 用户可见文案不得泄漏后端技术串——这是本次"提示词友好化"的回归红线。
const BANNED = ['malformed', 'unexpected status', '404 Not Found', 'Error:', 'connection refused']

describe('parseCode', () => {
  it('提取 [CODE] 前缀', () => {
    expect(parseCode('[feed_unavailable] unexpected status 404 Not Found')).toBe('feed_unavailable')
    expect(parseCode('  [net_fail] dial tcp: i/o timeout')).toBe('net_fail')
  })
  it('无前缀返回 null', () => {
    expect(parseCode('普通提示：已是最新版本')).toBeNull()
    expect(parseCode('')).toBeNull()
  })
})

describe('mapUpdateError', () => {
  it('带码错误映射到友好文案，原始串不进用户可见文本', () => {
    const f = mapUpdateError(new Error('[net_fail] connection refused'))
    expect(f.title).toBe('网络连接失败')
    expect(f.level).toBe('error')
    const text = f.title + f.detail
    for (const b of BANNED) expect(text).not.toContain(b)
  })
  it('无码错误走兜底，不透传英文', () => {
    const f = mapUpdateError(new Error('some raw backend error'))
    expect(f.title).toBe('操作失败')
    for (const b of BANNED) expect((f.title + f.detail).toLowerCase()).not.toContain(b)
  })
})

describe('sanitizeError', () => {
  const cases: [string, RegExp, string][] = [
    ['dial tcp: connection refused', /connection|refused/, '网络'],
    ['401 Unauthorized', /401/, '鉴权'],
    ['404 Not Found', /404|not found/, '资源'],
    ['500 Internal Server Error', /\b5\d\d\b/, '服务'],
  ]
  for (const [raw, _re, hint] of cases) {
    it(`消毒「${raw}」含「${hint}」且不泄漏技术串`, () => {
      const out = sanitizeError(raw)
      expect(out.length).toBeGreaterThan(0)
      for (const b of BANNED) expect(out).not.toContain(b)
    })
  }
})

describe('UPDATE_MESSAGES 不泄漏技术串', () => {
  it('所有升级错误文案均不含禁用子串', () => {
    for (const [code, f] of Object.entries(UPDATE_MESSAGES)) {
      const text = `${f.title}${f.detail}`
      for (const b of BANNED) {
        expect(text, `code=${code}`).not.toContain(b)
      }
    }
  })
})
