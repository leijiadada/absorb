// Native Messaging 管道管理：连接、断开重连、心跳保活、桥端口查询。
// 管道由浏览器拉起宿主进程承载，本模块只持有现役管道引用。

const NATIVE_HOST_NAME = 'com.absorb.desktop';
const NATIVE_RECONNECT_DELAY_MS = 5000;

// 宿主连接状态分类（供"未安装引导"判定）：
//   installed      connectNative 成功（注册表有宿主 = 已安装）
//   not-installed  connectNative 失败且错误为"宿主不存在"（注册表无宿主）
//   unknown        其他失败（权限被禁等，无法定性，宁漏报不误报）
// "宿主不存在"的真机实测文案因浏览器/版本而异（2026-11 真机验证）：
//   Edge / Chromium 现行："Specified native messaging host not found."
//   Chrome 系历史文案："No such native application com.absorb.desktop"
// 用正则同时覆盖两种，避免按单一精确串匹配在新版本浏览器上漏判。
export const HOST_MISSING_RE = /no such native application|native messaging host not found/i;

export class NativeBridge {
  constructor() {
    this.port = null; // 现役 NM 管道
    this.connected = false;
    this.reconnectTimer = null; // 挂起的重连定时器，connect 入口清理
    this._portRequest = null; // 并发去重：同一时刻只发一次 get-port
    this.hostStatus = 'unknown'; // 最近一次 connect 结果分类（见顶部说明）
  }

  connect() {
    // 已持有管道时直接返回：每次多余的 connectNative 都会让浏览器再拉起一个宿主
    // 进程，而孤儿管道只能靠宿主静默超时回收，重复建连会让宿主进程数持续堆积
    if (this.port) return;
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    try {
      const port = chrome.runtime.connectNative(NATIVE_HOST_NAME);
      this.port = port;
      this.connected = true;
      this.hostStatus = 'installed';
      // 桌面端重启后宿主经端口文件变化发 probe 主动唤醒 SW，回 ack 触发宿主重报
      // connect（连接状态秒级恢复）；宿主把任意入站消息都当作重报信号
      port.onMessage.addListener((msg) => {
        if (msg && msg.type === 'probe') {
          try { port.postMessage({ type: 'probe-ack' }); } catch (e) { /* 管道正断开，下轮 alarm 补连 */ }
        }
      });
      port.onDisconnect.addListener(() => this._onDisconnect(port));
      console.log('[Absorb] Native host 已连接');
    } catch (e) {
      console.warn('[Absorb] Native host 连接失败:', e?.message || e);
      this.connected = false;
      // 宿主不存在 = 桌面端未安装（安装器负责写注册表）；其他错误无法定性
      this.hostStatus = HOST_MISSING_RE.test(e?.message || '') ? 'not-installed' : 'unknown';
    }
  }

  _onDisconnect(port) {
    // 只响应现役管道的断开：旧管道残留的断开事件不清现役引用、不调度重连
    if (this.port !== port) return;
    const err = chrome.runtime.lastError?.message || 'unknown';
    console.warn('[Absorb] Native host 断开:', err);
    this.port = null;
    this.connected = false;
    // 未注册注册表（开发环境）等场景 connectNative 会立即失败，不无限重连，
    // 交由 30s 保活 alarm 周期性重试；宿主不存在时同步更新安装状态分类
    if (HOST_MISSING_RE.test(err)) {
      this.hostStatus = 'not-installed';
    } else {
      this.reconnectTimer = setTimeout(() => this.connect(), NATIVE_RECONNECT_DELAY_MS);
    }
  }

  // 心跳保活：周期消息保持 SW 与管道活跃（宿主回 pong 无需处理）；断开则补连
  heartbeat() {
    if (this.connected && this.port) {
      try { this.port.postMessage({ type: 'ping' }); } catch (e) { /* 管道正断开，下轮补连 */ }
    } else {
      this.connect();
    }
  }

  // 向 Native 宿主请求桥端口：宿主读数据根端口文件回 {type:'bridge',port}。
  // 宿主未连接/应答超时/消息异常时返回 0。
  requestBridgePort() {
    if (this._portRequest) return this._portRequest;
    this._portRequest = new Promise((resolve) => {
      const port = this.port;
      if (!port) { resolve(0); return; }
      let settled = false;
      const finish = (value) => {
        if (settled) return;
        settled = true;
        port.onMessage.removeListener(onMsg);
        this._portRequest = null;
        resolve(value);
      };
      const onMsg = (msg) => {
        if (!msg || msg.type !== 'bridge') return; // 只认 get-port 应答，忽略 pong/probe 等其他消息
        finish(typeof msg.port === 'number' ? msg.port : 0);
      };
      port.onMessage.addListener(onMsg);
      try {
        port.postMessage({ type: 'get-port' });
      } catch (e) {
        finish(0);
        return;
      }
      setTimeout(() => finish(0), 1500); // 宿主无应答兜底
    });
    return this._portRequest;
  }
}
