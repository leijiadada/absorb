# 商店权限 Justification 文案（CWS / Edge Partner Center）

> 用法：Chrome Web Store 开发者后台 Privacy practices 标签的每个权限有 justification 输入框（约 1000 字符上限，英文为准）；
> Edge Partner Center 的 "DOM API access" / 权限说明字段同理。英文直接粘贴，中文留档对照。
> Single purpose 必须一句话概括，两店都用下面这段。

## Single Purpose（单一用途声明）

**EN:**

> Absorb is a reading assistant: select text or interpret the full page you are reading, ask follow-up questions, and get AI answers grounded in that page. It does nothing else.

**中文：** 划词提问与整页解读的阅读助手：针对你正在阅读的网页进行划词提问、多轮追问与 AI 解读，无其他用途。

---

## 权限逐条 Justification

### contextMenus

**EN:**

> Adds a right-click menu entry ("Absorb interpret") so the user can start a selection-based question directly from the page. No data is read by the menu itself.

**中文：** 右键菜单提供"Absorb 解读"入口，用户可从页面直接发起划词提问；菜单本身不读取任何数据。

### sidePanel

**EN:**

> The side panel is the extension's main UI: the chat surface where questions are typed and AI answers are streamed. No page data is accessed through this permission.

**中文：** 侧边栏是扩展主界面，承载提问与流式回答的对话流；此权限不涉及页面数据读取。

### storage

**EN:**

> Stores only local UI state in chrome.storage.local: unsent drafts, a retry queue for selections not yet delivered to the local desktop app, document-card metadata, and user preferences. Nothing is synced to any server.

**中文：** 仅在 chrome.storage.local 保存本地状态：未发送草稿、待送达本地桌面端的划词重试队列、文档卡片元数据与用户偏好；不上传任何服务器。

### activeTab + scripting

**EN:**

> Lets the extension inject its capture script into the current tab only after the user explicitly triggers an action (right-click selection, panel button, or screenshot). The script reads the selected text and the page's main content to build the question context. No injection happens without user action.

**中文：** 仅当用户明确触发（右键划词、面板按钮、截图）时，向当前标签页注入抓取脚本，读取选中文本与页面正文主内容以构建提问上下文；无用户操作不注入。

### tabs

**EN:**

> Reads the URL and title of the active tab to attach page context to the user's question and to detect PDF pages (to switch to the PDF reading mode). Only the active tab, only url/title metadata — no page content is read via this permission and no browsing history is collected.

**中文：** 读取活动标签页的 URL 与标题，用于给提问附加页面上下文、识别 PDF 页面（切换 PDF 解读模式）；仅活动标签页、仅 url/title 元数据，不读取页面内容，不收集浏览历史。

### clipboardWrite

**EN:**

> Enables the one-click "copy answer" button in the chat panel. Writes to the clipboard only when the user clicks the copy button.

**中文：** 支持对话面板中"一键复制回答"；仅在用户点击复制按钮时写入剪贴板。

### alarms

**EN:**

> Maintains a periodic heartbeat (every 30 seconds) to detect whether the local desktop companion app is reachable, and retries delivering selections queued while the desktop app was closed. No data is transmitted externally.

**中文：** 每 30 秒心跳检测本地桌面应用可达性，并对桌面端未在线期间排队的划词进行重试投递；不向外传输数据。

### nativeMessaging

**EN:**

> Communicates with the "Absorb desktop" native messaging host installed locally by the Absorb desktop application (registered under HKCU by its installer). This is how the extension detects the companion app and exchanges connection state. All AI processing happens in the desktop app; the extension never sends data anywhere else.

**中文：** 与本地安装的 Absorb 桌面应用原生宿主通信（由桌面安装器写入 HKCU 注册表），用于检测桌面端可达性与连接状态；AI 处理全部在桌面端完成，扩展不向其他任何位置发送数据。

### host_permissions: http://localhost/* 与 http://127.0.0.1/*

**EN:**

> All extension network requests target the local bridge service of the companion desktop app on the user's own machine (localhost / 127.0.0.1). This permission is what guarantees the extension cannot talk to any remote server. There are no requests to any other origin.

**中文：** 扩展全部网络请求仅指向用户本机的桌面应用桥接服务（localhost/127.0.0.1），该权限保证了扩展无法与任何远程服务器通信；无其他来源的请求。

### host_permissions: <all_urls>（审核重点）

**EN:**

> The core feature — "select text on any page you are reading and ask about it" — requires reading the user-selected text and the page's main content in the page where the user is currently reading. This cannot be limited to a fixed site list by design: the whole point is that the user can use it on whatever they are reading right now.
>
> Scope control:
> - Injection into a page happens strictly on user action on that page (right-click selection, panel button, or screenshot gesture) via activeTab/scripting; there is no background or scheduled collection.
> - Only the current tab's selected text and main-content text are read; no links harvesting, no form data, no cross-tab crawling.
> - Captured content is sent only to the local companion desktop app on localhost, and from there only to the AI provider the user configures with their own API key.
> - The extension stores no browsing history and does not track users across sites.

**中文：** 核心功能是"在你正在阅读的任意网页上划词提问"，必须能在用户当前阅读的页面读取选中文本与正文主内容，这是产品定义决定的，无法收敛为固定站点清单。
范围控制：仅在用户于当前页主动操作时注入脚本（无后台/定时采集）；只读当前标签页的选中文本与正文，不抓链接、不读表单、不跨标签页爬取；内容只发往本机 localhost 桌面端，再由桌面端发给用户自行配置的 AI 服务商；不存储浏览历史，不做跨站追踪。

---

## CWS "Privacy practices" 问卷对照

| 问卷项 | 勾选 | 备注 |
|---|---|---|
| Does your item use remote hosted code? | **No** | 全部 JS 本地打包，mermaid 等为扩展内 vendor 文件懒加载 |
| Data collection: Website content | **Yes — collected, not shared to third parties... 见下** | 划词/正文属于 user content + website content |
| Personal communications (chat text) | Yes | 提问与回答文本 |
| User activity | No | 无浏览行为追踪 |
| Web browsing activity | No | 不收集浏览历史 |
| Do you sell or transfer data to third parties? | **No** | 唯一外部流向是用户自行配置的 AI 服务商，属"用户指令下的处理"，如实勾选并备注 |
| Do you use data for advertising? | No | |
| Do you use data for non-affiliate purposes? | No | |
| Privacy policy URL | 必填 | 托管 docs/store/privacy-policy.html 后填 URL |

> 注意：CWS 问卷若勾选"collect website content / communications"，会要求逐项说明用途与是否传给第三方——用上文 Single Purpose 与 <all_urls> justification 的措辞回答即可，口径保持一致：**只在用户主动触发时采集当页内容，只发往本机桌面端与用户自己配置的 AI 服务商，不做其他用途**。

## Edge Partner Center 对应字段

- **DOM API access**：说明同 activeTab/scripting + <all_urls> 条目
- **Native messaging**：说明同 nativeMessaging 条目（Edge 审核对 nativeMessaging + localhost 组合接受度高，桌面端依赖写清即可）
- **Privacy policy URL**：同 CWS
