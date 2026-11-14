// sync.js 会话镜像同步层测试：镜像上传（sessionId 守卫/full 剥离/失败静默）、
// 索引拉取（TTL/失败降级）、remoteOnlyStubs 归并去重、远端懒加载注入与缓存驱逐。
// sync.js → state.js（chrome.storage）/ net.js（chrome.runtime）依赖均经 vi.stubGlobal 注入。
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { state } from '../../../extension/sidepanel/js/state.js'
import {
  uploadMirror, refreshRemoteIndex, getRemoteIndex, remoteOnlyStubs, loadRemoteSession
} from '../../../extension/sidepanel/js/sync.js'

function stubChrome(port: number | null) {
  const store: Record<string, unknown> = {}
  const setSpy = vi.fn(async (obj: Record<string, unknown>) => { Object.assign(store, obj) })
  vi.stubGlobal('chrome', {
    runtime: { sendMessage: vi.fn(async () => ({ port: port ?? 0 })) },
    storage: {
      local: {
        get: vi.fn(async (keys: string | string[]) => {
          const list = Array.isArray(keys) ? keys : [keys]
          const out: Record<string, unknown> = {}
          for (const k of list) if (k in store) out[k] = store[k]
          return out
        }),
        set: setSpy,
      },
    },
  })
  return { store, setSpy }
}

function localSession(over: Record<string, unknown> = {}) {
  return {
    id: Math.random().toString(36).slice(2),
    sessionId: '',
    quotes: [],
    messages: [],
    createdAt: 1730000000000,
    updatedAt: 1730000000000,
    ...over,
  }
}

const indexItem = (over: Record<string, unknown> = {}) => ({
  sessionId: 'srv-1',
  title: '第一条提问',
  msgCount: 2,
  createdAt: new Date(1730000000000).toISOString(),
  updatedAt: new Date(1730000100000).toISOString(),
  ...over,
})

beforeEach(() => {
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
  state.sessions = []
  state.currentSessionId = null
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('uploadMirror — 镜像上传', () => {
  it('有 sessionId 时 PUT 整会话对象，内存态原图 full 被剥离', async () => {
    const fetchMock = vi.fn(async () => ({ ok: true, status: 200 }))
    vi.stubGlobal('fetch', fetchMock)
    stubChrome(7788)
    const s = localSession({
      sessionId: 'srv-1',
      messages: [{ role: 'user', images: [{ thumb: 't', full: 'data:image/png;base64,原始图' }] }],
    })
    uploadMirror(s)
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1))
    const [url, opts] = fetchMock.mock.calls[0]
    expect(url).toBe('http://localhost:7788/api/ext/sessions/srv-1/mirror')
    expect(opts.method).toBe('PUT')
    const body = JSON.parse(opts.body)
    expect(body.messages[0].images[0].full).toBeUndefined()
    expect(body.messages[0].images[0].thumb).toBe('t')
  })

  it('无 sessionId（从未提问）不上传', () => {
    const fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
    stubChrome(7788)
    uploadMirror(localSession({ sessionId: '' }))
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('网络失败静默：不抛出、不写日志之外的状态', async () => {
    const fetchMock = vi.fn(async () => { throw new TypeError('Failed to fetch') })
    vi.stubGlobal('fetch', fetchMock)
    stubChrome(7788)
    expect(() => uploadMirror(localSession({ sessionId: 'srv-1' }))).not.toThrow()
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1))
  })
})

describe('refreshRemoteIndex — 索引拉取', () => {
  it('全量拉取并归一化为 stub 结构', async () => {
    stubChrome(7788)
    vi.stubGlobal('fetch', vi.fn(async () => ({
      ok: true,
      json: async () => ({ items: [indexItem()] }),
    })))
    const idx = await refreshRemoteIndex(true)
    expect(idx).toHaveLength(1)
    expect(getRemoteIndex()[0]).toMatchObject({
      sessionId: 'srv-1', title: '第一条提问', msgCount: 2, updatedAt: 1730000100000,
    })
  })

  it('桌面不可达返回 null，索引清空', async () => {
    stubChrome(0)
    vi.stubGlobal('fetch', vi.fn())
    expect(await refreshRemoteIndex(true)).toBeNull()
    expect(getRemoteIndex()).toEqual([])
  })

  it('TTL 内重复调用不发请求，force 跳过 TTL', async () => {
    stubChrome(7788)
    const fetchMock = vi.fn(async () => ({
      ok: true,
      json: async () => ({ items: [indexItem()] }),
    }))
    vi.stubGlobal('fetch', fetchMock)
    await refreshRemoteIndex(true)
    await refreshRemoteIndex() // TTL 内：命中缓存
    expect(fetchMock).toHaveBeenCalledTimes(1)
    await refreshRemoteIndex(true) // force：重新拉取
    expect(fetchMock).toHaveBeenCalledTimes(2)
  })
})

describe('remoteOnlyStubs — 归并去重', () => {
  it('本地已有的 sessionId 不产生 stub，本地优先', async () => {
    stubChrome(7788)
    state.sessions = [localSession({ sessionId: 'srv-1' })]
    vi.stubGlobal('fetch', vi.fn(async () => ({
      ok: true,
      json: async () => ({ items: [indexItem(), indexItem({ sessionId: 'srv-2' })] }),
    })))
    await refreshRemoteIndex(true)
    const stubs = remoteOnlyStubs()
    expect(stubs).toHaveLength(1)
    expect(stubs[0].sessionId).toBe('srv-2')
  })
})

describe('loadRemoteSession — 懒加载注入', () => {
  it('详情注入本地缓存：fromRemote 标记、本地 id 重生成、saveState 落盘', async () => {
    const { setSpy } = stubChrome(7788)
    vi.stubGlobal('fetch', vi.fn(async (url: string) => {
      if (!url.includes('/api/ext/sessions/srv-1')) throw new Error('unexpected url ' + url)
      return {
        ok: true,
        json: async () => ({
          sessionId: 'srv-1',
          session: {
            sessionId: 'srv-1',
            quotes: [{ id: 1, text: '划词' }],
            messages: [{ role: 'user', content: '提问' }, { role: 'assistant', content: '回答' }],
            createdAt: 1730000000000,
            updatedAt: 1730000100000,
          },
        }),
      }
    }))
    const before = state.sessions.length
    const loaded = await loadRemoteSession('srv-1')
    expect(loaded).not.toBeNull()
    expect(loaded!.sessionId).toBe('srv-1')
    expect(loaded!.fromRemote).toBe(true)
    expect(loaded!.id).not.toBe('srv-1')
    expect(state.sessions).toHaveLength(before + 1)
    expect(setSpy).toHaveBeenCalled()
  })

  it('响应缺 session.messages 返回 null，不注入', async () => {
    stubChrome(7788)
    vi.stubGlobal('fetch', vi.fn(async () => ({
      ok: true,
      json: async () => ({ sessionId: 'srv-1', session: null }),
    })))
    expect(await loadRemoteSession('srv-1')).toBeNull()
    expect(state.sessions).toHaveLength(0)
  })

  it('缓存达上限时驱逐远端缓存；无远端可逐时回退驱逐最旧本地会话', async () => {
    stubChrome(7788)
    // 预填 50 条本地会话（无 fromRemote），应全部保留
    state.sessions = Array.from({ length: 50 }, () => localSession())
    let n = 0
    vi.stubGlobal('fetch', vi.fn(async () => ({
      ok: true,
      json: async () => ({
        sessionId: 'srv-' + n,
        session: { sessionId: 'srv-' + n++, messages: [{ role: 'user', content: 'x' }] },
      }),
    })))
    // 无远端缓存可逐：回退驱逐最旧本地，总量守在 50
    const first = await loadRemoteSession('srv-0')
    expect(first).not.toBeNull()
    expect(state.sessions).toHaveLength(50)
    expect(state.sessions.filter((s) => !s.fromRemote)).toHaveLength(49)
    // 再注入一条远端：超出上限，驱逐之前那条 fromRemote，本地 49 条不动
    const locals = state.sessions.filter((s) => !s.fromRemote)
    await loadRemoteSession('srv-99')
    expect(state.sessions.filter((s) => !s.fromRemote)).toHaveLength(49)
    expect(state.sessions.filter((s) => s.fromRemote)).toHaveLength(1)
    expect(locals.every((l) => state.sessions.includes(l))).toBe(true)
  })
})
