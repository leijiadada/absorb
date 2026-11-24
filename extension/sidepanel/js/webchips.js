// webchips.js —— 普通网页的建议提问 chips（doc-suggestions-design.md §4.4）：
// 活动标签页非注册文档类型（DOC_TYPES 未命中，普通网页无文档卡）时，内容区
// 尾部问题块（#webChips，位于提问框上方）显示 2~3 个基于页面章节标题的成句
// 问题——视觉归属内容区（灰底无框），与输入区的边界由 composer 白底分隔线产生。
// 素材 = grab.js 采集的 pageMd 标题行（客户端就地抽取，零服务端往返）；
// 组句 = 客户端轻模板（语言跟随文档/页面，不新增服务端 i18n）。
// 点击落输入框走 submitQuestion 全链路（无划词采集当前页注入 pageMd，回答
// 基于页面正文，不经 search_document——未 prepare）。
// 与文档卡互斥：PDF/注册文档类型活动页时不出现 chips（文档卡负责展示）。
// 与消息有无解耦：问题块常驻 composer（提问框上方），有消息时仍显示供追问
// 参考；chips 随活动标签页切换实时刷新。
import { detectDocType } from './doccard.js';
import { t } from './i18n.js';

const WEB_CHIP_MAX = 3; // chips 条数上限（页面标题抽前 3 个）
const WEB_CHIP_MIN = 2; // 低于此条数不渲染（宁缺勿凑）

let _ask = null;
let _chipsEl = null;
let _currentUrl = '';
let _seq = 0; // 采集代际：标签页快速切换时丢弃过期采集结果

function el(tag, cls, text) {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined) n.textContent = text;
  return n;
}

// extractTitles 抽 pageMd 标题行：#/## 优先（### 兜底兜住小节），去重、
// 去行内 markdown 修饰、40 rune 截断（与章节树标题同款上限）。
function extractTitles(pageMd) {
  const titles = [];
  const seen = new Set();
  const re = /^(#{1,3})\s+(.+?)\s*$/gm;
  let m;
  while ((m = re.exec(pageMd || '')) !== null && titles.length < WEB_CHIP_MAX) {
    let title = m[2].replace(/[*`_\[\]]/g, '').trim();
    if (title.length > 40) title = title.slice(0, 40);
    if (!title || seen.has(title)) continue;
    seen.add(title);
    titles.push(title);
  }
  return titles;
}

// capturePageMd 采集当前活动页 pageMd（与 conversation.getActivePage 同款注入
// 文件序列）。文档类型/非 http(s)/未加载完（导航中间态与错误页注入必败，等
// onUpdated status=complete 再触发）/注入失败 → null（静默：无 chips 宁缺勿错）。
async function capturePageMd() {
  try {
    const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
    if (!tab || !tab.url || !/^https?:\/\//i.test(tab.url)) return null;
    if (detectDocType(tab.url)) return null; // 文档类型走文档卡，互斥
    if (tab.status !== 'complete') return null; // 加载中/错误页：注入必败，等加载完成再试
    const results = await chrome.scripting.executeScript({
      target: { tabId: tab.id },
      files: ['sidepanel/vendor/defuddle.min.js', 'html-pruner.js', 'pageconvert.js', 'grab.js']
    });
    const r = results && results[0] && results[0].result;
    return r && r.pageMd ? r.pageMd : '';
  } catch (e) {
    console.warn('[Absorb] webchips 页面采集失败（该页无 chips，不影响提问）:', e);
    return null;
  }
}

function hide() {
  if (!_chipsEl) return;
  _chipsEl.hidden = true;
  _chipsEl.textContent = '';
}

function render(titles) {
  _chipsEl.hidden = false;
  _chipsEl.textContent = '';
  _chipsEl.append(el('div', 'db-sec wc-title', t('webchips.title')));
  titles.forEach((title) => {
    const text = t('webchips.question', { title });
    const chip = el('button', 'db-suggest-item wc-chip');
    chip.type = 'button';
    chip.append(el('span', 'db-suggest-text', text), el('span', 'arr', '→'));
    chip.addEventListener('click', () => { if (_ask) _ask(text); });
    _chipsEl.append(chip);
  });
}

// refresh 按活动标签页刷新 chips：注册文档类型 → 隐藏（文档卡负责）；
// 普通网页 → 采集 pageMd 抽标题渲染。采集代际防过期结果覆盖新页。
async function refresh() {
  if (!_chipsEl || !_ask) return;
  let url = '';
  try {
    const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
    if (!tab || !tab.url || detectDocType(tab.url)) {
      hide();
      return;
    }
    url = tab.url;
  } catch {
    hide();
    return;
  }
  _currentUrl = url;
  const seq = ++_seq;
  const pageMd = await capturePageMd();
  if (seq !== _seq || _currentUrl !== url) return; // 采集期间切页：丢弃过期结果
  const titles = extractTitles(pageMd);
  if (titles.length < WEB_CHIP_MIN) {
    hide();
    return;
  }
  render(titles);
}

// initWebChips（main.js 调用）：注入提问入口、挂标签页监听。ask 与文档卡同一
// 实现——文本追加进输入框后走 submitQuestion 全链路（追加而非清空）。
export async function initWebChips({ ask }) {
  _ask = ask || null;
  _chipsEl = document.getElementById('webChips');
  if (!_chipsEl) return;
  await refresh();
  chrome.tabs.onActivated.addListener(() => { refresh(); });
  chrome.tabs.onUpdated.addListener((_tabId, info) => {
    // url/title 变化导航中必败（status 守卫拦截）；加载完成信号单独触发补跑
    if (info.url || info.title || info.status === 'complete') refresh();
  });
}
