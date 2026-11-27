// 扩展测试共享 chrome 桩：storage.local 语义（backing 过滤读取 + 可编程 set 失败）
// 与 runtime.sendMessage 桥端口应答统一在此维护。各 spec 手写桩的字段语义曾出现
// 漂移（get 按 has 还是 in 过滤、backing 用 Map 还是 Record），chrome API 加字段
// 时需多处同改——收敛为一份后只改这里。
// 只做纯构造不做 stubGlobal，由各 spec 自行挂载（挂载时机与还原策略归用例管）。
import { vi } from 'vitest'

export interface StorageStub {
  /** 预置/断言存储内容的背板 */
  backing: Map<string, unknown>
  /** storage.local.set 的可编程桩（未传 setImpl 时默认写回 backing） */
  set: ReturnType<typeof vi.fn>
  /** chrome.storage.local 形状 */
  local: {
    get: (keys: string | string[]) => Promise<Record<string, unknown>>
    set: (obj: Record<string, unknown>) => Promise<void>
  }
}

export function storageStub(setImpl?: (obj: Record<string, unknown>) => Promise<void>): StorageStub {
  const backing = new Map<string, unknown>()
  const defaultSet = async (obj: Record<string, unknown>) => {
    for (const [k, v] of Object.entries(obj)) backing.set(k, v)
  }
  const set = vi.fn(setImpl ?? defaultSet)
  const local = {
    get: vi.fn(async (keys: string | string[]) => {
      const ks = Array.isArray(keys) ? keys : [keys]
      const out: Record<string, unknown> = {}
      for (const k of ks) if (backing.has(k)) out[k] = backing.get(k)
      return out
    }),
    set,
  }
  return { backing, set, local }
}

/** runtime.sendMessage 桥端口发现桩：port 为桌面端下发的桥端口（null 语义同 0=未就绪） */
export function bridgeRuntimeStub(port: number | null) {
  return {
    runtime: {
      sendMessage: vi.fn(async () => ({ port })),
    },
  }
}
