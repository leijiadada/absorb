// 桌面端未安装引导：探测 Native 宿主注册状态，未安装时在空态区渲染下载引导卡片。
// 探测只读（SW connectNative 分类 + get-port），不拉起任何进程；已安装（无论桌面端
// 运行与否）一律不显示——"未启动"由重连条负责，避免误导用户重装。
import { els } from './render.js';
import { getHostStatus } from './net.js';
import { t } from './i18n.js';
import { DESKTOP_DOWNLOAD_URL } from './config.js';

// 探测并按结果显隐卡片。detecting=true 时由"重新检测"按钮触发（按钮进检测中态）。
async function detect(detecting = false) {
  const btn = els.dgRecheck;
  if (detecting && btn) {
    // 先按当前文案锁定最小宽度再换短文案（"检测中…"）：
    // 按钮行是 justify-content:center，宽度变化会让整行重新居中、两个按钮一起跳动
    btn.style.minWidth = `${btn.offsetWidth}px`;
    btn.disabled = true;
    btn.textContent = t('desktopGuide.checking');
  }
  try {
    const status = await getHostStatus();
    els.desktopGuide.hidden = status !== 'not-installed';
  } catch (e) {
    console.warn('[Absorb] 桌面端安装状态探测失败:', e);
    // 探测异常按未知处理：不显示卡片（宁漏报不误报）
    els.desktopGuide.hidden = true;
  } finally {
    if (detecting && btn) {
      btn.disabled = false;
      btn.textContent = t('desktopGuide.recheck');
      btn.style.minWidth = ''; // 文案复原后解锁：残留的 min-width 在面板变窄时会撑破容器
    }
  }
}

// init 接入：绑定按钮 + 首次探测（不阻塞面板主流程，调用方无需 await）。
// 卡片每次打开面板由 init 重新探测——用户装完桌面端后重开面板即自然恢复。
export function initDesktopGuide() {
  els.dgDownload.addEventListener('click', () => {
    chrome.tabs.create({ url: DESKTOP_DOWNLOAD_URL });
  });
  els.dgRecheck.addEventListener('click', () => detect(true));
  detect();
}
