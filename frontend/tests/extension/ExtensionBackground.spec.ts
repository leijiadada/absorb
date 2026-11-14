import { describe, expect, it, vi, beforeEach } from 'vitest'

// background 模块顶层即执行 connectNative/alarms 注册，需先装 chrome 桩再动态导入入口
function stubChrome() {
  // 每次 connectNative 产出独立端口对象（ports[0] 为 SW 顶层连接），供守卫用例区分现役/旧管道
  const ports: PortStub[] = []
  const makePort = (): PortStub => ({
    postMessage: vi.fn(),
    onMessage: { addListener: vi.fn(), removeListener: vi.fn() },
    onDisconnect: { addListener: vi.fn() },
  })
  const chrome = {
    runtime: {
      connectNative: vi.fn(() => {
        const p = makePort()
        ports.push(p)
        return p
      }),
      onMessage: { addListener: vi.fn() },
      onInstalled: { addListener: vi.fn() },
    },
    alarms: { create: vi.fn(), onAlarm: { addListener: vi.fn() } },
    contextMenus: { create: vi.fn(), removeAll: vi.fn(), onClicked: { addListener: vi.fn() } },
    sidePanel: { setPanelBehavior: vi.fn(() => Promise.resolve()) },
    i18n: { getMessage: vi.fn() },
    windows: { WINDOW_ID_CURRENT: -2 },
    // E2E 测试钩子依赖：session.get 默认返回空（未预写 override，走 Native 管道原路径）
    storage: { session: { get: vi.fn(async () => ({})) } },
  }
  return { chrome, ports, get port() { return ports[0] } }
}

interface PortStub {
  postMessage: ReturnType<typeof vi.fn>
  onMessage: { addListener: ReturnType<typeof vi.fn>; removeListener: ReturnType<typeof vi.fn> }
  onDisconnect: { addListener: ReturnType<typeof vi.fn> }
}

async function loadBackground() {
  const ctx = stubChrome()
  vi.stubGlobal('chrome', ctx.chrome)
  await import('../../../extension/background/background.js')
  return ctx
}

// 端口钩子链路含 storage.session 查询，多一层微任务；setTimeout(0) 完整刷空
function flushMicrotasks() {
  return new Promise((r) => setTimeout(r, 0))
}

beforeEach(() => {
  vi.resetModules()
  vi.unstubAllGlobals()
})

describe('Native probe 应答', () => {
  it('收到 probe 回 probe-ack（宿主据此重报 connect）', async () => {
    const { port } = await loadBackground()
    const probeListener = port.onMessage.addListener.mock.calls.at(-1)![0] as (m: unknown) => void
    probeListener({ type: 'probe' })
    expect(port.postMessage).toHaveBeenCalledWith({ type: 'probe-ack' })
  })

  it('非 probe 消息（pong/bridge）不触发应答', async () => {
    const { port } = await loadBackground()
    const probeListener = port.onMessage.addListener.mock.calls.at(-1)![0] as (m: unknown) => void
    probeListener({ type: 'pong' })
    probeListener({ type: 'bridge', port: 8901 })
    probeListener(null)
    expect(port.postMessage).not.toHaveBeenCalled()
  })
})

describe('get-port 应答过滤', () => {
  it('只认 type=bridge 的应答，probe-ack 等消息不误判', async () => {
    const { port, chrome } = await loadBackground()
    const runtimeListener = chrome.runtime.onMessage.addListener.mock.calls[0][0] as (
      msg: unknown,
      sender: unknown,
      respond: (r: unknown) => void,
    ) => boolean
    const respond = vi.fn()
    expect(runtimeListener({ type: 'get-bridge-port' }, {}, respond)).toBe(true)
    // 端口钩子先查 storage.session（异步），需先刷空微任务，get-port 应答 listener 才注册
    await flushMicrotasks()

    const tempListener = port.onMessage.addListener.mock.calls.at(-1)![0] as (m: unknown) => void
    tempListener({ type: 'probe-ack' }) // 非应答消息：不 resolve
    await flushMicrotasks()
    expect(respond).not.toHaveBeenCalled()

    tempListener({ type: 'bridge', port: 8901 })
    await flushMicrotasks()
    // 响应含 hostStatus（未安装引导判定源）：stub 的 connectNative 成功 → installed
    expect(respond).toHaveBeenCalledWith({ port: 8901, hostStatus: 'installed' })
    expect(port.postMessage).toHaveBeenCalledWith({ type: 'get-port' })
  })

  it('connectNative 失败按错误分类 hostStatus：宿主不存在=not-installed', async () => {
    const ctx = stubChrome()
    const { NativeBridge } = await import('../../../extension/background/native-connection.js')
    // 真机实测（2026-11）两种"宿主不存在"文案均须命中：
    // Edge/Chromium 现行文案
    ctx.chrome.runtime.connectNative = vi.fn(() => {
      throw new Error('Specified native messaging host not found.')
    })
    vi.stubGlobal('chrome', ctx.chrome)
    const b = new NativeBridge()
    b.connect()
    expect(b.hostStatus).toBe('not-installed')
    expect(b.connected).toBe(false)

    // Chrome 系历史文案
    ctx.chrome.runtime.connectNative = vi.fn(() => {
      throw new Error('No such native application com.absorb.desktop')
    })
    const b1 = new NativeBridge()
    b1.connect()
    expect(b1.hostStatus).toBe('not-installed')

    // 其他错误无法定性 → unknown（宁漏报不误报）
    ctx.chrome.runtime.connectNative = vi.fn(() => {
      throw new Error('Access to the specified native messaging host is forbidden')
    })
    const b2 = new NativeBridge()
    b2.connect()
    expect(b2.hostStatus).toBe('unknown')
  })

  it('connectNative 成功 → hostStatus=installed', async () => {
    const { chrome } = await loadBackground()
    expect(chrome.runtime.connectNative).toHaveBeenCalledTimes(1)
    const { NativeBridge } = await import('../../../extension/background/native-connection.js')
    const b = new NativeBridge()
    b.connect()
    expect(b.hostStatus).toBe('installed')
  })
})

// storage.local 桩：set 可编程失败（模拟配额溢出），get 从 backing Map 读取。
// 不传 setImpl 时 set 默认写回 backing（正常路径用例依赖）。
// 模块级定义：存储降级 / quote-queue / 划词消费多组用例共用。
function stubStorage(setImpl?: (obj: Record<string, unknown>) => Promise<void>) {
  const backing = new Map<string, unknown>()
  const defaultSet = async (obj: Record<string, unknown>) => {
    for (const [k, v] of Object.entries(obj)) backing.set(k, v)
  }
  const set = vi.fn(setImpl ?? defaultSet)
  const chromeStub = {
    storage: {
      local: {
        get: vi.fn(async (keys: string | string[]) => {
          const ks = Array.isArray(keys) ? keys : [keys]
          const out: Record<string, unknown> = {}
          for (const k of ks) if (backing.has(k)) out[k] = backing.get(k)
          return out
        }),
        set,
      },
    },
  }
  return { backing, set, chromeStub }
}

describe('存储降级（配额防护）', () => {
  it('appendPendingQuote 入队失败时剥离 pageMd 降级重试，保住划词文本', async () => {
    const calls: Record<string, unknown>[] = []
    let failFirst = true
    const { chromeStub } = stubStorage(async (obj) => {
      calls.push(obj)
      if (failFirst) { failFirst = false; throw new Error('QUOTA_BYTES exceeded') }
    })
    vi.stubGlobal('chrome', chromeStub)
    const { appendPendingQuote } = await import('../../../extension/background/quote-queue.js')
    await appendPendingQuote({ text: '术语解释', pageMd: '超大页面的结构化文本' })
    expect(calls.length).toBe(2)
    const list = calls[1].pendingQuotes as Array<{ text: string; pageMd: string }>
    expect(list[0].text).toBe('术语解释')
    expect(list[0].pageMd).toBe('')
  })

  it('appendPendingQuote 正常路径整包入队（含 pageMd）', async () => {
    const { chromeStub, backing } = stubStorage()
    vi.stubGlobal('chrome', chromeStub)
    const { appendPendingQuote } = await import('../../../extension/background/quote-queue.js')
    await appendPendingQuote({ text: '术语', pageMd: '页面文本' })
    const list = backing.get('pendingQuotes') as Array<{ pageMd: string }>
    expect(list).toHaveLength(1)
    expect(list[0].pageMd).toBe('页面文本')
  })

  it('appendPendingQuote 同文未消费划词跳过入队', async () => {
    const { chromeStub, backing } = stubStorage(async () => {})
    backing.set('pendingQuotes', [{ id: 500, text: '同文', url: 'http://a', pageMd: 'x' }])
    backing.set('pendingCursor', 100)
    vi.stubGlobal('chrome', chromeStub)
    const { appendPendingQuote } = await import('../../../extension/background/quote-queue.js')
    await appendPendingQuote({ text: '同文', url: 'http://a', pageMd: 'y' })
    expect(backing.get('pendingQuotes') as unknown[]).toHaveLength(1)
  })

  it('saveState 配额失败时剥离 pageMd 重试且不抛出', async () => {
    const calls: Record<string, unknown>[] = []
    let failFirst = true
    const { chromeStub } = stubStorage(async (obj) => {
      calls.push(obj)
      if (failFirst) { failFirst = false; throw new Error('QUOTA_BYTES exceeded') }
    })
    vi.stubGlobal('chrome', chromeStub)
    const { saveState, state } = await import('../../../extension/sidepanel/js/state.js')
    state.sessions.push({ id: 's1', sessionId: '', quotes: [{ id: 1, text: 't', pageMd: '<big>' }], messages: [], createdAt: 1, updatedAt: 1 })
    await expect(saveState()).resolves.toBeUndefined()
    expect(calls.length).toBe(2)
    const slim = calls[1]['absorb-state'] as { sessions: Array<{ quotes: Array<{ pageMd: string }> }> }
    expect(slim.sessions[0].quotes[0].pageMd).toBe('')
  })

  it('saveState 两次失败也不抛出（主流程不被炸断）', async () => {
    const { chromeStub } = stubStorage(async () => { throw new Error('QUOTA_BYTES exceeded') })
    vi.stubGlobal('chrome', chromeStub)
    const { saveState } = await import('../../../extension/sidepanel/js/state.js')
    await expect(saveState()).resolves.toBeUndefined()
  })
})

describe('重连竞态守卫', () => {
  it('alarm 抢先补连后 5s timer 不再建连，旧管道断开不触发重连', async () => {
    vi.useFakeTimers()
    try {
      const { chrome, ports } = await loadBackground()
      expect(chrome.runtime.connectNative).toHaveBeenCalledTimes(1) // SW 顶层连接

      const alarmListener = chrome.alarms.onAlarm.addListener.mock.calls[0][0] as (a: { name: string }) => void
      // 现役管道（ports[0]）断开 → 调度 5s 重连
      const disc0 = ports[0].onDisconnect.addListener.mock.calls[0][0] as () => void
      disc0()
      // 5s 内 alarm 抢先补连成功
      vi.advanceTimersByTime(1000)
      alarmListener({ name: 'absorb-heartbeat' })
      expect(chrome.runtime.connectNative).toHaveBeenCalledTimes(2)
      // 已被 connectNativeHost 入口清理的 5s timer 到点不再建连
      vi.advanceTimersByTime(10000)
      expect(chrome.runtime.connectNative).toHaveBeenCalledTimes(2)

      // 现役管道（ports[1]）在位时，旧管道（ports[0]）残留断开事件是空操作
      disc0()
      vi.advanceTimersByTime(60000)
      expect(chrome.runtime.connectNative).toHaveBeenCalledTimes(2)

      // 现役管道断开仍正常调度重连
      const disc1 = ports[1].onDisconnect.addListener.mock.calls[0][0] as () => void
      disc1()
      vi.advanceTimersByTime(5000)
      expect(chrome.runtime.connectNative).toHaveBeenCalledTimes(3)
    } finally {
      vi.useRealTimers()
    }
  })
})

describe('quote-queue 容量与 id', () => {
  it('超过 30 条上限时裁掉最旧的（保尾部最新划词）', async () => {
    const { chromeStub, backing } = stubStorage()
    vi.stubGlobal('chrome', chromeStub)
    const { appendPendingQuote } = await import('../../../extension/background/quote-queue.js')
    for (let i = 0; i < 35; i++) await appendPendingQuote({ text: '词' + i, url: 'http://a' })
    const list = backing.get('pendingQuotes') as Array<{ text: string }>
    expect(list).toHaveLength(30)
    expect(list[0].text).toBe('词5') // 最旧 5 条被裁
    expect(list[29].text).toBe('词34')
  })

  it('同毫秒连续入队 id 仍严格单调递增', async () => {
    const { chromeStub, backing } = stubStorage()
    vi.stubGlobal('chrome', chromeStub)
    const { appendPendingQuote } = await import('../../../extension/background/quote-queue.js')
    for (let i = 0; i < 3; i++) await appendPendingQuote({ text: '词' + i, url: 'http://a' })
    const list = backing.get('pendingQuotes') as Array<{ id: number }>
    expect(list).toHaveLength(3)
    expect(list[0].id).toBeLessThan(list[1].id)
    expect(list[1].id).toBeLessThan(list[2].id)
  })

  it('已被消费（id ≤ 游标）的同文划词放行重新入队（删除标签后可再次引用）', async () => {
    const { chromeStub, backing } = stubStorage(async () => {})
    backing.set('pendingQuotes', [{ id: 50, text: '同文', url: 'http://a', pageMd: '' }])
    backing.set('pendingCursor', 100)
    vi.stubGlobal('chrome', chromeStub)
    const { appendPendingQuote } = await import('../../../extension/background/quote-queue.js')
    await appendPendingQuote({ text: '同文', url: 'http://a', pageMd: 'y' })
    const list = backing.get('pendingQuotes') as Array<{ id: number }>
    expect(list).toHaveLength(2)
    expect(list[1].id).toBeGreaterThan(100) // 新条目 id 必须大于游标才能被再次消费
  })
})

describe('划词消费（游标推进 + 串行尾链）', () => {
  it('流式中跳过消费，游标不推进', async () => {
    const { chromeStub, backing } = stubStorage()
    backing.set('pendingQuotes', [{ id: 10, text: '甲', url: '', pageMd: '' }])
    vi.stubGlobal('chrome', chromeStub)
    const { consumePendingQuotes, setStreaming } = await import('../../../extension/sidepanel/js/state.js')
    setStreaming(true)
    await expect(consumePendingQuotes()).resolves.toBe(false)
    expect(backing.has('pendingCursor')).toBe(false) // 游标未推进
  })

  it('消费按 id 升序并入当前会话，游标推进到最大 id', async () => {
    const { chromeStub, backing } = stubStorage()
    backing.set('pendingQuotes', [
      { id: 100, text: '甲', url: '', pageMd: '' },
      { id: 50, text: '乙', url: '', pageMd: '' }, // 乱序入队
    ])
    vi.stubGlobal('chrome', chromeStub)
    const { consumePendingQuotes } = await import('../../../extension/sidepanel/js/state.js')
    const fresh = await consumePendingQuotes()
    expect((fresh as Array<{ id: number }>).map((q) => q.id)).toEqual([50, 100])
    expect(backing.get('pendingCursor')).toBe(100)
    const st = backing.get('absorb-state') as { sessions: Array<{ quotes: Array<{ id: number }> }> }
    expect(st.sessions[0].quotes.map((q) => q.id)).toEqual([50, 100])
  })

  it('并发入口经串行尾链去重，不重复消费同一划词', async () => {
    const { chromeStub, backing } = stubStorage()
    backing.set('pendingQuotes', [
      { id: 10, text: '甲', url: '', pageMd: '' },
      { id: 20, text: '乙', url: '', pageMd: '' },
    ])
    vi.stubGlobal('chrome', chromeStub)
    const { consumePendingQuotes } = await import('../../../extension/sidepanel/js/state.js')
    const p1 = consumePendingQuotes()
    const p2 = consumePendingQuotes() // 与 p1 并发：若不串行化会读到同一游标重复消费
    const [r1, r2] = await Promise.all([p1, p2])
    expect((r1 as Array<{ id: number }>).map((q) => q.id)).toEqual([10, 20])
    expect(r2).toBeNull() // 第二次调用读到已推进的游标 → 无增量
    const st = backing.get('absorb-state') as { sessions: Array<{ quotes: unknown[] }> }
    expect(st.sessions[0].quotes).toHaveLength(2) // 无重复 chip
  })
})
