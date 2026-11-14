// Absorb 阅读助手 - 后台 Service Worker 入口：装配各模块并注册事件。
console.log('[Absorb] Service Worker 启动');

import { NativeBridge } from './native-connection.js';
import { registerMenu, handleMenuClick } from './quote-collector.js';

const bridge = new NativeBridge();
bridge.connect(); // SW 每次被唤醒立即连接

// sidepanel 端口查询：SW 经 Native 管道 get-port 后回 {port}（0=桌面端未就绪）；
// hostStatus 为宿主连接分类（installed/not-installed/unknown，未安装引导判定源）
chrome.runtime.onMessage.addListener((msg, _sender, sendResponse) => {
  if (!msg || msg.type !== 'get-bridge-port') return;
  // E2E 测试钩子：storage.session 预写 testBridgePort/testHostStatus 时绕过 Native
  // 管道（生产无写入，不触发）；testHostStatus 缺省按 installed（测试宿主均在册）
  chrome.storage.session.get(['testBridgePort', 'testHostStatus']).then(({ testBridgePort, testHostStatus }) => {
    if (testBridgePort > 0 || testHostStatus) {
      return sendResponse({ port: testBridgePort || 0, hostStatus: testHostStatus || 'installed' });
    }
    return bridge.requestBridgePort().then((port) => {
      sendResponse({ port, hostStatus: bridge.hostStatus });
    });
  });
  return true; // 异步 sendResponse
});

// E2E 测试钩子：模拟右键菜单点击，走与 handleMenuClick 完全相同的采集链。
// 原生菜单在页面 DOM 之外无法自动化；仅扩展自身页面可发此消息，生产不发送。
chrome.runtime.onMessage.addListener((msg, _sender, sendResponse) => {
  if (!msg || msg.type !== 'test-menu-click') return;
  // skipPanelOpen：E2E 自开面板页，禁开原生 side panel（多面板实例会竞争消费划词队列）
  handleMenuClick(msg.info, msg.tab, !!msg.skipPanelOpen).then(() => sendResponse({ ok: true }));
  return true; // 异步 sendResponse
});

// 管道保活心跳（对齐 Trae browser-bridge 心跳模式）
const HEARTBEAT_ALARM = 'absorb-heartbeat';
chrome.alarms.create(HEARTBEAT_ALARM, { periodInMinutes: 0.5 });
chrome.alarms.onAlarm.addListener((a) => {
  if (a.name === HEARTBEAT_ALARM) bridge.heartbeat();
});

// 面板行为每次 SW 唤醒时设置（幂等）：仅挂 onInstalled 时，浏览器重启后可能丢失"点图标开面板"
chrome.sidePanel.setPanelBehavior({ openPanelOnActionClick: true }).catch(() => {});

chrome.runtime.onInstalled.addListener(() => {
  registerMenu();
});

chrome.contextMenus.onClicked.addListener(handleMenuClick);
