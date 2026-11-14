// Absorb 阅读助手 - 国际化文案
// 职责：集中管理 sidepanel 全部面向用户文案，按浏览器语言切换中英文。
// manifest 元数据与右键菜单 title 走 chrome.i18n 原生机制（_locales/ 目录），
// 与此处的 key 分开维护，命名尽量对齐。加语言：新增语言表对象并接入 selectTable。

const zh = {
  // 错误提示（异常态）
  errors: {
    desktopNotReady: '桌面端未就绪\n\n请先启动 Absorb 桌面端程序，再重新提问。',
    desktopMissing: '尚未安装 Absorb 桌面端\n\n请先下载安装桌面端程序，再重新提问。',
    serviceNotRunning: '助手服务未启动\n\n请先启动本机的 Absorb 助手程序，再重新提问。',
    connectionTimeout: '连接超时\n\n助手服务响应过慢，请稍后重试。',
    serverError: '助手服务出错了\n\n请稍后重试，若持续出现请重启助手程序。',
    requestRejected: '请求无法处理\n\n请检查输入后重试，若持续出现请重启助手程序。',
    agentError: '助手服务返回了错误\n\n请稍后重试，若持续出现请重启助手程序。',
    unknown: '发生未知错误，请稍后重试。',
    streamBroken: '网络中断，回答不完整',
    testConnFailed: '无法连接，请检查服务是否启动',
    testConnError: '服务返回异常（{status}）',
    knowledge: {
      saveFailed: '保存失败，请稍后重试。内容已保留，可直接重试。',
    },
    screenshotUnsupported: '当前页面不支持截图，请改用上传或粘贴图片。',
    screenshotFailed: '截图失败，请重试或改用上传图片。',
  },
  // 知识保存面板（M2 吸收闭环）
  knowledge: {
    entry: '把这次理解留下来',
    entrySummary: '保存这次摘要',
    summarySaved: '摘要已保存',
    summarySaveFailed: '摘要保存失败，请重试。',
    title: '吸收这次理解',
    subtitle: '将值得保留的内容变成自己的知识',
    titleLabel: '标题',
    titleHint: '· AI 建议，可修改',
    quoteLabel: '原文摘录',
    quoteHint: '· 保留证据',
    summaryLabel: 'AI 提炼',
    summaryHint: '· 可编辑',
    understandingLabel: '我的理解',
    understandingHint: '· 选填',
    understandingPlaceholder: '这对我有什么启发？与我的项目或已有认识有什么关系？',
    draftDegraded: 'AI 提炼暂不可用，已留空，可手写后保存',
    noQuoteNote: '本轮没有划词摘录，将以对话轮次作为来源记录',
    sourceLabel: '来源：',
    sourceOpen: '当前文章 · 打开原文',
    cancel: '取消',
    save: '保存到我的知识',
    saving: '保存中…',
    saved: '✓ 已保存',
    savedDup: '已保存过',
    savedDupNote: '这条理解此前已保存过，以下是当前记录。如需更新，请重新总结后再保存。',
    savedMeta: '保存于刚刚',
    close: '关闭',
    done: '完成',
    saveFailed: '保存失败。内容已保留，可直接重试。',
    titleRequired: '标题不能为空，请填写后再保存。',
    assocTitle: '与你之前的理解有关（{n}）',
    assocYouThink: '你认为：',
    assocYouSaved: '你保存过：',
    assocCompare: '比较新旧理解',
    assocComparePrompt: '这篇内容与我之前对「{title}」的理解有什么不同？',
  },
  // 按稳定错误码查表（服务端 errs.Code），前端据此渲染本地化文案
  errorsByCode: {
    'model.image_unsupported': '当前模型不支持图片输入，请删除图片后重试，或切换到支持视觉的模型',
    'model.auth_invalid': 'API 密钥无效或未授权，请检查密钥',
    'model.rate_limited': '请求过于频繁或额度受限，请稍后重试',
    'model.quota_exceeded': '账户额度不足，请充值后重试',
    'model.context_too_long': '内容超出模型上下文长度上限',
    'model.not_found': '模型不存在，请检查模型配置',
    'model.permission_denied': '无权限访问该模型或接口，请检查模型配置或套餐',
    'model.content_policy': '请求或回答被内容安全策略拦截',
    'model.bad_request': '请求格式有误，请检查输入后重试',
    'model.timeout': '请求超时，请稍后重试',
    'model.network': '无法连接服务，请检查网络与代理设置',
    'model.unavailable': '服务暂不可用，请稍后重试',
    'agent.internal': '服务内部错误，请稍后重试',
  },
  // 过程提示（进行态）
  process: {
    thinking: '思考中…',
    thinkingDone: '思考过程',
    testing: '测试中…',
  },
  // ask_user 提问卡片（官方 HITL：模型提问即终止本轮，回答经 /api/ask/answer 续答）
  ask: {
    placeholder: '输入你的回答…',
    send: '发送',
  },
  // 可视化解释
  visual: {
    generating: '可视化页面生成中…',
    degraded: '可视化页面渲染失败，以下为原始内容',
  },
  // 软提示（建议性）
  soft: {
    roundsWarn: '已 {rounds} 轮对话，建议点「＋新」新建会话以节省 token（历史会话保留在列表中）',
  },
  // 占位提示
  placeholders: {
    inputWithQuote: '针对引用提问，例如：这句话什么意思？',
    inputNoQuote: '右键划词后，针对引用提问',
  },
  // 空态
  empty: {
    historyEmpty: '暂无历史对话',
    sessionEmpty: '（空对话）',
  },
  // 桌面端未安装引导卡片（desktop-guide.js 渲染于空态区）
  desktopGuide: {
    title: '需要 Absorb 桌面端',
    desc: '未检测到桌面端（可能未安装，或正在升级/更新）。插件负责划词与提问，AI 回答由本机桌面端完成。',
    download: '下载桌面端',
    recheck: '我已安装，重新检测',
    checking: '检测中…',
  },
  // 文档卡（PDF 解读：未读取/读取中/已就绪/失败四态）
  doccard: {
    label: '当前文档',
    badgeIdle: '尚未读取',
    badgeReading: '正在读取',
    badgeReady: '已就绪',
    badgeNeedAuth: '需要登录',
    badgeTooLarge: '文件过大',
    badgeNoText: '无可读文本',
    badgeEncrypted: '已加密',
    badgeInvalidUrl: '链接无效',
    badgeError: '读取失败',
    metaSource: 'PDF 文档 · 当前浏览器标签页',
    descIdle: '读取文档正文后，Absorb 才能基于整份文档进行解读和追问。',
    read: '读取文档',
    retry: '重试',
    step1: '正在检查 PDF 来源',
    step2: '正在下载文档',
    step3: '正在提取正文',
    processing: '正在处理文档，请稍候……',
    metaReady: '正文已提取 · {chars} 字 · 文本层可读取',
    metaPartial: '正文部分提取 · {chars} 字',
    suggestTitle: '从理解这份文档开始',
    suggest1: '帮我建立这篇文档的整体理解框架',
    suggest2: '提炼核心观点、论据和结论',
    suggest3: '有哪些观点值得质疑或进一步思考？',
    reread: '重新读取',
    openOriginal: '打开原文',
    errorGeneric: '读取失败，请稍后重试。',
    errorTimeout: '读取超时，请检查网络后重试。',
  },
  // 按钮文案
  buttons: {
    ask: '发送',
    stop: '停止',
    stopped: '⏹ 已停止',
    retry: '重试',
    reconnect: '重连',
    newSession: '新会话',
    continue: '继续',
  },
  // 代码块复制
  copy: {
    copy: '复制',
    copied: '✓ 已复制',
  },
  // 图表全屏查看器
  lightbox: {
    zoomIn: '放大（滚轮）',
    zoomOut: '缩小（滚轮）',
    reset: '重置视图（适配窗口）',
    close: '关闭（Esc / 点击空白）',
    downsampled: '历史缩略图（原图不保留，点击放大查看）',
    originalNotKept: '原图不保留，可重新截图核对',
  },
  // 图卡片工具栏（复制/下载/新窗口三键）
  fig: {
    copy: '复制图片',
    download: '下载 PNG',
    open: '新窗口打开',
    copyFailed: '图片复制失败，请改用下载',
    exportFailed: '图片导出失败，请重试',
  },
  // HTML 静态文案（sidepanel.html 经 data-i18n* 属性替换）
  ui: {
    docTitle: 'Absorb 阅读助手',
    history: '历史',
    newShort: '＋新',
    historyTip: '查看历史',
    restoreHistory: '拉取历史会话',
    remoteLoadFailed: '加载失败，请确认桌面端已启动',
    newTip: '新对话',
    historyTitle: '历史对话',
    emptyTip1: '右键划词后,会以标签插入输入框;可多段划词,针对标签提问即可。',
    emptyTip2: '回答将在此流式显示。',
    removeChip: '移除该标签',
    webSource: '网页',
    me: '我',
    imageTip: '插入图片',
    imageDisabledTip: '当前模型不支持图片输入',
    imageMenuTitle: '选择图片来源',
    imageRegion: '区域截图',
    imageElement: '元素截图',
    imageViewport: '视口截图',
    imageUpload: '上传图片',
    imagePaste: '粘贴图片',
    imageRemove: '移除图片',
    imageCompressing: '处理中…',
    imageCropHint: '拖拽选择区域，或按 Esc 取消',
  },
};

const en = {
  errors: {
    desktopNotReady: 'Desktop not ready\n\nStart the Absorb desktop app, then ask again.',
    desktopMissing: 'Absorb desktop app is not installed\n\nDownload and install the desktop app first, then ask again.',
    serviceNotRunning: 'The assistant service is not running\n\nStart the local Absorb assistant, then ask again.',
    connectionTimeout: 'Connection timed out\n\nThe assistant service is slow to respond. Please try again later.',
    serverError: 'The assistant service returned an error\n\nPlease try again later. If it persists, restart the assistant.',
    requestRejected: 'The request cannot be processed\n\nCheck your input and try again.',
    agentError: 'The assistant service reported an error\n\nPlease try again later. If it persists, restart the assistant.',
    unknown: 'An unknown error occurred. Please try again later.',
    streamBroken: 'Network interrupted — the answer is incomplete',
    testConnFailed: 'Cannot connect. Check that the service is running',
    testConnError: 'The service returned an error ({status})',
    knowledge: {
      saveFailed: 'Save failed. Please try again later. Your content is kept for retry.',
    },
    screenshotUnsupported: 'Screenshots are not supported on this page. Use upload or paste instead.',
    screenshotFailed: 'Screenshot failed. Retry, or use upload instead.',
  },
  knowledge: {
    entry: 'Keep this insight',
    entrySummary: 'Save this summary',
    summarySaved: 'Summary saved',
    summarySaveFailed: 'Failed to save summary. Try again.',
    title: 'Absorb this insight',
    subtitle: 'Turn what is worth keeping into your own knowledge',
    titleLabel: 'Title',
    titleHint: '· AI suggested, editable',
    quoteLabel: 'Quote',
    quoteHint: '· evidence kept',
    summaryLabel: 'AI summary',
    summaryHint: '· editable',
    understandingLabel: 'My understanding',
    understandingHint: '· optional',
    understandingPlaceholder: 'What does this inspire? How does it relate to my work or what I already know?',
    draftDegraded: 'AI summary unavailable — left empty, you can write your own',
    noQuoteNote: 'No quote in this round; this conversation turn will be recorded as the source',
    sourceLabel: 'Source: ',
    sourceOpen: 'current article · open original',
    cancel: 'Cancel',
    save: 'Save to my knowledge',
    saving: 'Saving…',
    saved: '✓ Saved',
    savedDup: 'Already saved',
    savedDupNote: 'This insight was saved before. Here is the current record. To update it, summarize again and save as new.',
    savedMeta: 'Saved just now',
    close: 'Close',
    done: 'Done',
    saveFailed: 'Save failed. Your content is kept — retry anytime.',
    titleRequired: 'Title cannot be empty. Fill it in before saving.',
    assocTitle: 'Related to your past understanding ({n})',
    assocYouThink: 'You noted: ',
    assocYouSaved: 'You saved: ',
    assocCompare: 'Compare past & new',
    assocComparePrompt: 'How does this differ from my past understanding of "{title}"?',
  },
  errorsByCode: {
    'model.image_unsupported': 'The current model does not support image input. Remove the image and retry, or switch to a vision-capable model.',
    'model.auth_invalid': 'API key is invalid or unauthorized. Check your key.',
    'model.rate_limited': 'Too many requests or quota exceeded. Try again later.',
    'model.quota_exceeded': 'Account quota exceeded. Top up and retry.',
    'model.context_too_long': 'Content exceeds the model context length limit.',
    'model.not_found': 'Model not found. Check your model configuration.',
    'model.permission_denied': 'No permission for this model or API. Check model config or plan.',
    'model.content_policy': 'Request or response blocked by content policy.',
    'model.bad_request': 'Bad request. Check your input and retry.',
    'model.timeout': 'Request timed out. Try again later.',
    'model.network': 'Cannot connect to the service. Check network and proxy settings.',
    'model.unavailable': 'Service temporarily unavailable. Try again later.',
    'agent.internal': 'Internal server error. Try again later.',
  },
  process: {
    thinking: 'Thinking…',
    thinkingDone: 'Thought process',
    testing: 'Testing…',
  },
  ask: {
    placeholder: 'Type your answer…',
    send: 'Send',
  },
  visual: {
    generating: 'Rendering visual explanation…',
    degraded: 'Visual rendering failed, showing raw content below',
  },
  soft: {
    roundsWarn: '{rounds} rounds in this conversation. Start a new one to save tokens (history stays in the list)',
  },
  placeholders: {
    inputWithQuote: 'Ask about the quoted text, e.g. What does this sentence mean?',
    inputNoQuote: 'Select text on a page, then right-click to ask',
  },
  empty: {
    historyEmpty: 'No history yet',
    sessionEmpty: '(empty conversation)',
  },
  // Desktop-not-installed guide card (rendered by desktop-guide.js in the empty state)
  desktopGuide: {
    title: 'Absorb desktop app required',
    desc: 'Desktop app not detected (not installed, or being upgraded). The extension handles selection and asking; AI answers run in the local desktop app.',
    download: 'Download desktop app',
    recheck: 'Already installed? Re-check',
    checking: 'Checking…',
  },
  // Document card (PDF reading: idle / reading / ready / error states)
  doccard: {
    label: 'Current document',
    badgeIdle: 'Not read yet',
    badgeReading: 'Reading',
    badgeReady: 'Ready',
    badgeNeedAuth: 'Sign-in required',
    badgeTooLarge: 'File too large',
    badgeNoText: 'No readable text',
    badgeEncrypted: 'Encrypted',
    badgeInvalidUrl: 'Invalid URL',
    badgeError: 'Read failed',
    metaSource: 'PDF document · current browser tab',
    descIdle: 'Absorb needs the document text before it can interpret and follow up on the full document.',
    read: 'Read document',
    retry: 'Retry',
    step1: 'Checking PDF source',
    step2: 'Downloading document',
    step3: 'Extracting text',
    processing: 'Processing the document, please wait…',
    metaReady: 'Text extracted · {chars} chars · text layer readable',
    metaPartial: 'Text partially extracted · {chars} chars',
    suggestTitle: 'Start by understanding this document',
    suggest1: 'Build an overall framework of this document for me',
    suggest2: 'Extract the core points, arguments and conclusions',
    suggest3: 'Which viewpoints are worth questioning or thinking further about?',
    reread: 'Read again',
    openOriginal: 'Open original',
    errorGeneric: 'Read failed. Please try again later.',
    errorTimeout: 'Read timed out. Check your network and retry.',
  },
  buttons: {
    ask: 'Send',
    stop: 'Stop',
    stopped: '⏹ Stopped',
    retry: 'Retry',
    reconnect: 'Reconnect',
    newSession: 'New session',
    continue: 'Continue',
  },
  copy: {
    copy: 'Copy',
    copied: '✓ Copied',
  },
  lightbox: {
    zoomIn: 'Zoom in (scroll)',
    zoomOut: 'Zoom out (scroll)',
    reset: 'Reset view (fit to window)',
    close: 'Close (Esc / click outside)',
    downsampled: 'Historical thumbnail (original not kept, click to enlarge)',
    originalNotKept: 'Original image is not kept. Take a new screenshot if needed.',
  },
  // Figure card toolbar (copy / download / open-in-tab)
  fig: {
    copy: 'Copy image',
    download: 'Download PNG',
    open: 'Open in new tab',
    copyFailed: 'Copy failed. Try download instead.',
    exportFailed: 'Export failed. Try again.',
  },
  ui: {
    docTitle: 'Absorb Reading Assistant',
    history: 'History',
    newShort: '＋ New',
    historyTip: 'View history',
    restoreHistory: 'Pull history',
    remoteLoadFailed: 'Load failed. Make sure the desktop app is running',
    newTip: 'New conversation',
    historyTitle: 'History',
    emptyTip1: 'Select text and right-click to pin it as a chip in the input box. Pin multiple selections and ask about them together.',
    emptyTip2: 'Answers will stream in here.',
    removeChip: 'Remove chip',
    webSource: 'Web',
    me: 'Me',
    imageTip: 'Insert image',
    imageDisabledTip: 'The current model does not support image input',
    imageMenuTitle: 'Choose image source',
    imageRegion: 'Region screenshot',
    imageElement: 'Element screenshot',
    imageViewport: 'Viewport screenshot',
    imageUpload: 'Upload image',
    imagePaste: 'Paste image',
    imageRemove: 'Remove image',
    imageCompressing: 'Processing…',
    imageCropHint: 'Drag to select an area, or press Esc to cancel',
  },
};

// 跟随浏览器语言：zh 开头（zh-CN/zh-TW…）用中文，其余用英文。
// chrome.i18n 仅扩展上下文可用；普通页面（开发桩页）无此 API，回退 zh-CN
const uiLang = (typeof chrome !== 'undefined' && chrome.i18n && chrome.i18n.getUILanguage)
  ? chrome.i18n.getUILanguage()
  : 'zh-CN';
const messages = /^zh/i.test(uiLang) ? zh : en;

export { messages, uiLang };

// 按服务端稳定错误码返回本地化文案；未登记时返回 undefined，由调用方回退通用提示。
export function errorByCode(code) {
  return messages.errorsByCode?.[code];
}

// 模板取值：t('errors.serverError') / t('process.thinkingDone', { count: 120 })
export function t(key, params = {}) {
  const msg = key.split('.').reduce((o, k) => o?.[k], messages);
  if (typeof msg !== 'string') return key;
  return msg.replace(/\{(\w+)\}/g, (_, k) => params[k] ?? '');
}

// 把 HTML 静态文案按当前语言替换（浏览器不会替换 HTML 里的 __MSG_*__，只能 JS 注入）：
// data-i18n → textContent；data-i18n-placeholder → 写回 data-placeholder
// （contenteditable 占位由 CSS attr(data-placeholder) 渲染）；
// data-i18n-title → title，且原有 aria-label 一并同步
export function applyDomI18n(root = document) {
  document.documentElement.lang = uiLang;
  root.querySelectorAll('[data-i18n]').forEach((el) => {
    el.textContent = t(el.dataset.i18n);
  });
  root.querySelectorAll('[data-i18n-placeholder]').forEach((el) => {
    el.dataset.placeholder = t(el.dataset.i18nPlaceholder);
  });
  root.querySelectorAll('[data-i18n-title]').forEach((el) => {
    const v = t(el.dataset.i18nTitle);
    el.title = v;
    if (el.getAttribute('aria-label')) el.setAttribute('aria-label', v);
  });
}
