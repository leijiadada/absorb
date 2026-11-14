// net.js 网络层测试：fetchWithRetry 重试分类/退避/外部中止、classifyNetError 文案映射、
// callAgent SSE 流解析（token/reasoning/session/done/断流/错误事件）与 payload 构造。
// net.js 为 ESM，静态导入即可；其依赖 i18n.js 在无 chrome 环境回退 zh-CN → 断言用中文文案。
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { fetchWithRetry, classifyNetError, callAgent, getBridgePort } from '../../../extension/sidepanel/js/net.js'

function abortError() {
  return new DOMException('The operation was aborted', 'AbortError')
}

beforeEach(() => {
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

afterEach(() => {
  vi.useRealTimers()
})

describe('fetchWithRetry — 重试分类', () => {
  it('连接类错误（TypeError）重试至多 attempts 次后抛出，退避间隔生效', async () => {
    vi.useFakeTimers()
    const fetchMock = vi.fn(() => Promise.reject(new TypeError('Failed to fetch')))
    vi.stubGlobal('fetch', fetchMock)

    const p = fetchWithRetry('http://x/api', {}, { attempts: 3, delays: [1000, 2000] })
    const assertion = expect(p).rejects.toThrow(TypeError)
    await vi.advanceTimersByTimeAsync(0) // 第 1 次尝试（创建即发起）
    expect(fetchMock).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(1000) // 1s 退避后第 2 次
    expect(fetchMock).toHaveBeenCalledTimes(2)
    await vi.advanceTimersByTimeAsync(2000) // 2s 退避后第 3 次
    await assertion
    expect(fetchMock).toHaveBeenCalledTimes(3) // 不超过 attempts
  })

  it('连接超时（AbortError）属于可重试错误：超时后重试，第二次成功', async () => {
    vi.useFakeTimers()
    const fetchMock = vi
      .fn()
      .mockImplementationOnce((_url: string, opts: RequestInit) =>
        // 挂起不返回，模拟 connectTimeoutMs 到点 abort 后 reject
        new Promise((_resolve, reject) => {
          opts.signal!.addEventListener('abort', () => reject(abortError()))
        }))
      .mockImplementationOnce(() => Promise.resolve({ ok: true, status: 200 }))
    vi.stubGlobal('fetch', fetchMock)

    const p = fetchWithRetry('http://x/api', {}, { attempts: 3, delays: [1000, 2000], connectTimeoutMs: 5000 })
    const assertion = expect(p).resolves.toMatchObject({ ok: true })
    await vi.advanceTimersByTimeAsync(5000) // 连接超时触发，第 1 次 reject
    await vi.advanceTimersByTimeAsync(1000) // 退避后第 2 次成功
    await assertion
    expect(fetchMock).toHaveBeenCalledTimes(2)
  })

  it('非连接类错误（HTTP 逻辑错误）不重试，直接抛出', async () => {
    const fetchMock = vi.fn(() => Promise.reject(new Error('boom')))
    vi.stubGlobal('fetch', fetchMock)
    await expect(fetchWithRetry('http://x/api', {}, { attempts: 3 })).rejects.toThrow('boom')
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })

  it('外部主动中止后不再重试（区分"停止"与"真错误"）', async () => {
    vi.useFakeTimers()
    const fetchMock = vi.fn(() => Promise.reject(new TypeError('Failed to fetch')))
    vi.stubGlobal('fetch', fetchMock)
    const ext = new AbortController()

    const p = fetchWithRetry('http://x/api', {}, { attempts: 3, delays: [1000], externalSignal: ext.signal })
    const assertion = expect(p).rejects.toThrow()
    await vi.advanceTimersByTimeAsync(0) // 第 1 次失败 → 排入 1s 退避
    ext.abort() // 用户点"停止"
    await vi.advanceTimersByTimeAsync(1000) // 第 2 次失败 → 侦测到外部中止，不再重试
    await assertion
    expect(fetchMock).toHaveBeenCalledTimes(2) // 未耗尽 attempts
  })

  it('HTTP 状态码错误不在此层重试：原样返回响应交调用方分类', async () => {
    const resp = { ok: false, status: 500 }
    const fetchMock = vi.fn(() => Promise.resolve(resp))
    vi.stubGlobal('fetch', fetchMock)
    await expect(fetchWithRetry('http://x/api', {})).resolves.toBe(resp)
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })
})

describe('classifyNetError — 错误文案映射', () => {
  beforeEach(() => {
    // e.message 分支会 console.error 技术细节，测试输出静默
    vi.spyOn(console, 'error').mockImplementation(() => {})
  })

  it('AbortError → 连接超时文案', () => {
    expect(classifyNetError(abortError())).toContain('连接超时')
  })

  it('TypeError → 服务未启动文案', () => {
    expect(classifyNetError(new TypeError('Failed to fetch'))).toContain('助手服务未启动')
  })

  it('已本地化的 Error.message 原样透传', () => {
    expect(classifyNetError(new Error('后端预检提示'))).toBe('后端预检提示')
  })

  it('无信息错误兜底通用文案', () => {
    expect(classifyNetError({})).toContain('发生未知错误')
  })
})

describe('getBridgePort — 桥端口发现', () => {
  it('sendMessage 失败时返回 0（桌面端未就绪语义）', async () => {
    vi.stubGlobal('chrome', {
      runtime: { sendMessage: vi.fn(() => Promise.reject(new Error('no SW'))) },
    })
    await expect(getBridgePort()).resolves.toBe(0)
  })

  it('应答缺 port 字段时返回 0', async () => {
    vi.stubGlobal('chrome', {
      runtime: { sendMessage: vi.fn(() => Promise.resolve({})) },
    })
    await expect(getBridgePort()).resolves.toBe(0)
  })
})

// ---------- callAgent ----------

function stubChromePort(port: number) {
  vi.stubGlobal('chrome', {
    runtime: { sendMessage: vi.fn(async () => ({ port })) },
  })
}

// SSE 响应桩：chunks 逐段经 body.getReader() 产出
function sseResponse(chunks: string[]) {
  const encoder = new TextEncoder()
  let i = 0
  return {
    ok: true,
    status: 200,
    body: {
      getReader: () => ({
        read: async () =>
          i < chunks.length
            ? { done: false, value: encoder.encode(chunks[i++]) }
            : { done: true, value: undefined },
      }),
    },
  }
}

describe('callAgent — 桥端口与 HTTP 前置', () => {
  it('端口未知（0）时直接按桌面端未就绪抛出，不发请求', async () => {
    stubChromePort(0)
    const fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
    await expect(callAgent('问题', [], '', vi.fn(), vi.fn())).rejects.toThrow('桌面端未就绪')
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('HTTP 500 不重试，抛服务端错误文案', async () => {
    stubChromePort(8901)
    const fetchMock = vi.fn(() => Promise.resolve({ ok: false, status: 500 }))
    vi.stubGlobal('fetch', fetchMock)
    await expect(callAgent('问题', [], '', vi.fn(), vi.fn())).rejects.toThrow('助手服务出错了')
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })
})

describe('callAgent — SSE 流解析', () => {
  it('token/reasoning/session 事件分发回调，done 收尾 broken=false', async () => {
    stubChromePort(8901)
    const fetchMock = vi.fn(() =>
      Promise.resolve(sseResponse([
        'data: {"type":"reasoning","content":"想"}\n\n',
        'data: {"type":"token","content":"答"}\n\ndata: {"type":"session","sessionId":"s9"}\n\n',
        'data: {"done":true}\n\n',
      ])))
    vi.stubGlobal('fetch', fetchMock)

    const onToken = vi.fn()
    const onReasoning = vi.fn()
    const onSession = vi.fn()
    const r = await callAgent('问题', [], '', onToken, onReasoning, undefined, onSession)
    // done 返回值含 requestId（保存知识回传）与 associations（关联卡片，无命中为 null）；
    // ask_user 时 done 另含 pending/askUser（挂起提问，普通轮恒为 false/null）
    expect(r).toEqual({ sessionId: 's9', requestId: '', associations: null, pending: false, askUser: null, broken: false })
    expect(onReasoning).toHaveBeenCalledWith('想')
    expect(onToken).toHaveBeenCalledWith('答')
    expect(onSession).toHaveBeenCalledWith('s9')
  })

  it('associations 事件（done 前下发）被捕获并随 done 返回', async () => {
    stubChromePort(8901)
    vi.stubGlobal('fetch', vi.fn(() =>
      Promise.resolve(sseResponse([
        'data: {"type":"token","content":"答"}\n\n',
        'data: {"type":"associations","requestId":"r7","kind":"summary","candidates":[{"itemId":"k1"}]}\n\n',
        'data: {"done":true,"requestId":"r7"}\n\n',
      ]))))
    const r = await callAgent('问题', [], '', vi.fn(), vi.fn())
    expect(r).toEqual({
      sessionId: '',
      requestId: 'r7',
      associations: { kind: 'summary', candidates: [{ itemId: 'k1' }] },
      pending: false,
      askUser: null,
      broken: false,
    })
  })

  it('ask_user 卡片帧被捕获，done 携带 pending:true（官方 HITL 挂起）', async () => {
    stubChromePort(8901)
    vi.stubGlobal('fetch', vi.fn(() =>
      Promise.resolve(sseResponse([
        'data: {"type":"ask_user","ticketId":"t1","question":"你想了解哪种？","options":["B+ 树","LSM 树"]}\n\n',
        'data: {"done":true,"requestId":"r8","pending":true}\n\n',
      ]))))
    const r = await callAgent('问题', [], '', vi.fn(), vi.fn())
    expect(r).toEqual({
      sessionId: '',
      requestId: 'r8',
      associations: null,
      pending: true,
      askUser: { ticketId: 't1', question: '你想了解哪种？', options: ['B+ 树', 'LSM 树'] },
      broken: false,
    })
  })

  it('流结束未收到 done → broken=true（断流信号）', async () => {
    stubChromePort(8901)
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(sseResponse(['data: {"type":"token","content":"半"}\n\n']))))
    const r = await callAgent('问题', [], '', vi.fn(), vi.fn())
    expect(r).toEqual({ sessionId: '', broken: true })
  })

  it('error 事件带稳定错误码 → 走本地化码表', async () => {
    stubChromePort(8901)
    vi.stubGlobal('fetch', vi.fn(() =>
      Promise.resolve(sseResponse(['data: {"type":"error","code":"model.auth_invalid"}\n\n']))))
    await expect(callAgent('问题', [], '', vi.fn(), vi.fn())).rejects.toThrow('API 密钥无效或未授权')
  })

  it('error 事件无码 → 回退 evt.message（后端自带中文提示）', async () => {
    stubChromePort(8901)
    vi.stubGlobal('fetch', vi.fn(() =>
      Promise.resolve(sseResponse(['data: {"type":"error","message":"预检：模型未配置"}\n\n']))))
    await expect(callAgent('问题', [], '', vi.fn(), vi.fn())).rejects.toThrow('预检：模型未配置')
  })

  it('非法 JSON 的 data 行被跳过不中断流', async () => {
    stubChromePort(8901)
    vi.stubGlobal('fetch', vi.fn(() =>
      Promise.resolve(sseResponse([
        'data: {broken json}\n\n',
        'data: {"type":"token","content":"答"}\n\n',
        'data: {"done":true}\n\n',
      ]))))
    const onToken = vi.fn()
    const r = await callAgent('问题', [], '', onToken, vi.fn())
    expect(r.broken).toBe(false)
    expect(onToken).toHaveBeenCalledTimes(1)
  })
})

describe('callAgent — payload 构造', () => {
  it('单条引用：[[1]] 占位替换为材料摘要锚点，context 取第一条', async () => {
    stubChromePort(8901)
    const fetchMock = vi.fn(() => Promise.resolve(sseResponse(['data: {"done":true}\n\n'])))
    vi.stubGlobal('fetch', fetchMock)

    await callAgent('这句话什么意思？[[1]]', [{ seq: 1, text: '一二三四五六七八九十一二三四五六七八九十（超长文本）', url: 'http://a' }], 's1', vi.fn(), vi.fn())
    const body = JSON.parse(fetchMock.mock.calls[0][1].body)
    expect(body.question).not.toContain('[[1]]')
    expect(body.question).toContain('…') // 摘要截断符
    expect(body.sessionId).toBe('s1')
    expect(body.context.text).toBe('一二三四五六七八九十一二三四五六七八九十（超长文本）')
  })

  it('多条引用：保留【引用n】编号并把全文拼进问题前', async () => {
    stubChromePort(8901)
    const fetchMock = vi.fn(() => Promise.resolve(sseResponse(['data: {"done":true}\n\n'])))
    vi.stubGlobal('fetch', fetchMock)

    await callAgent('对比 [[1]] 和 [[2]]', [
      { seq: 1, text: '第一条', url: 'http://a' },
      { seq: 2, text: '第二条', url: 'http://a' },
    ], '', vi.fn(), vi.fn())
    const body = JSON.parse(fetchMock.mock.calls[0][1].body)
    expect(body.question).toContain('【引用1】\n第一条')
    expect(body.question).toContain('【引用2】\n第二条')
    expect(body.question).toContain('对比 【引用1】 和 【引用2】')
    expect(body.context.text).toBe('第一条') // context 只取第一条
  })

  it('无划词但采集到当前页 → 构造纯页面 context 并附 page', async () => {
    stubChromePort(8901)
    const fetchMock = vi.fn(() => Promise.resolve(sseResponse(['data: {"done":true}\n\n'])))
    vi.stubGlobal('fetch', fetchMock)

    const page = { url: 'http://p', title: 'T', pageMd: '页面正文' }
    await callAgent('总结这一页', [], '', vi.fn(), vi.fn(), undefined, undefined, page)
    const body = JSON.parse(fetchMock.mock.calls[0][1].body)
    expect(body.context).toEqual({ text: '', paragraph: '', pageContext: '', url: 'http://p', pageMd: '页面正文' })
    expect(body.page).toEqual(page)
  })
})

// ---------- callAgentAnswer（ask_user 续答，官方 HITL） ----------

import { callAgentAnswer } from '../../../extension/sidepanel/js/net.js'

describe('callAgentAnswer — 续答流解析', () => {
  it('token 分发回调，done 返回 requestId', async () => {
    stubChromePort(8901)
    const fetchMock = vi.fn(() =>
      Promise.resolve(sseResponse([
        'data: {"type":"token","content":"续"}\n\n',
        'data: {"done":true,"requestId":"r9"}\n\n',
      ])))
    vi.stubGlobal('fetch', fetchMock)
    const onToken = vi.fn()
    const r = await callAgentAnswer('s1', 't1', 'LSM 树', onToken, vi.fn())
    expect(r).toEqual({ requestId: 'r9', broken: false })
    expect(onToken).toHaveBeenCalledWith('续')
    const body = JSON.parse(fetchMock.mock.calls[0][1].body)
    expect(body).toEqual({ sessionId: 's1', ticketId: 't1', answer: 'LSM 树' })
  })

  it('4xx 校验错误取 message 抛出（非 SSE）', async () => {
    stubChromePort(8901)
    vi.stubGlobal('fetch', vi.fn(() =>
      Promise.resolve({ ok: false, status: 400, json: async () => ({ code: 1, message: '会话不存在或已清理，无法继续回答' }) })))
    await expect(callAgentAnswer('s1', 't1', '答案')).rejects.toThrow('会话不存在或已清理')
  })

  it('流结束未收到 done → broken=true', async () => {
    stubChromePort(8901)
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(sseResponse(['data: {"type":"token","content":"半"}\n\n']))))
    const r = await callAgentAnswer('s1', 't1', '答案', vi.fn(), vi.fn())
    expect(r).toEqual({ requestId: '', broken: true })
  })
})
