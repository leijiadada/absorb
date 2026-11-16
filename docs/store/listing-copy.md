# 商店上架文案（CWS / Edge Add-ons）

> 口径三处一致：**只在用户主动触发时采集当页内容，只发往本机桌面端与用户自己配置的 AI 服务商，不做其他用途**。
> CWS 详细描述为纯文本（商店不渲染 Markdown），换行与符号已按纯文本排版设计。

---

## 名称（Name）

- 简体：`Absorb 阅读助手`
- 简明英文：`Absorb Reading Assistant`
- （manifest `__MSG_extName__` 已是同一口径，无需改动）

## 简短描述（Short description，CWS 上限 132 字符）

**简体：**

> 划词提问、整页解读、多轮追问。AI 结合选中句与网页正文回答，读技术文档、长文章、论文更快。需配合 Absorb 桌面端使用。

**EN:**

> Select text or interpret the full page and ask follow-up questions. AI answers grounded in the page you are reading. Requires the Absorb desktop app.

## 详细描述（Detailed description，纯文本）

**简体：**

```
Absorb 阅读助手——一边读，一边问。

专为"读技术文档、长文章、论文"设计的 AI 阅读助手。不用复制粘贴，划词即问；AI 结合你划选的句子与网页正文回答，答得更准、更贴上下文。

主要功能
● 划词提问：右键划选任意句子直接提问，AI 结合选中句与整页内容回答
● 整页解读：一键生成整页要点、结论与延伸问题
● PDF 解读：识别 PDF 页面，按文档模式解读长文档
● 多轮追问：同一会话连续追问，上下文不丢
● 图片提问：截图、上传或粘贴图片，针对图表细节提问
● 流式阅读体验：Markdown 排版、代码高亮、图表渲染，回答即出即读
● 知识沉淀：把有用回答一键存入本地知识库，随桌面端统一管理
● 会话历史：自动保存对话，重开面板继续上次话题

隐私与数据
● 扩展只在页面上不采集任何数据，全部功能由你主动触发
● 划词、页面内容与提问仅发送给你本机安装的 Absorb 桌面应用（localhost）
● AI 处理由桌面应用调用你自己配置的服务商与 API Key 完成，本扩展不经手任何密钥
● 不含广告、埋点与行为追踪；隐私政策见商店开发者信息

开始使用
1. 安装本扩展
2. 下载并安装 Absorb 桌面应用（面板内会给出下载引导），配置你的 AI 服务商
3. 打开任意网页，右键划词选择"Absorb 解读"，即可开始提问
```

**EN:**

```
Absorb Reading Assistant — read, and ask as you go.

An AI reading assistant built for technical docs, long articles, and papers. No copy-pasting: select any sentence and ask. Answers are grounded in your selection and the page content, so they stay accurate and in context.

Key features
● Select-to-ask: right-click any sentence and ask; AI answers using your selection plus the full page
● Full-page interpret: one click for key points, conclusions, and follow-up questions
● PDF reading: PDF pages are detected and interpreted in document mode
● Multi-turn follow-ups: keep asking in the same session with full context
● Image questions: screenshot, upload, or paste images and ask about charts and details
● Streaming reading experience: Markdown layout, code highlighting, and rendered diagrams
● Knowledge capture: save useful answers into your local knowledge base, managed in the desktop app
● Session history: conversations are saved; reopen the panel and pick up where you left off

Privacy & data
● The extension collects nothing on its own — every action is user-triggered
● Selections, page content, and questions go only to the Absorb desktop app on your own machine (localhost)
● AI processing is done by the desktop app through the provider and API key YOU configure; the extension never touches keys
● No ads, no telemetry, no behavioral tracking. Privacy policy: see the developer listing info

Getting started
1. Install this extension
2. Download and install the Absorb desktop app (the panel offers a download guide), then configure your AI provider
3. Open any web page, select text, right-click "Absorb interpret", and start asking
```

## 分类与标签

- CWS Category：`Productivity`（工具效率）；备选 `Tools`
- Edge Category：`Productivity`
- 关键词（备用）：划词提问 / 网页总结 / 阅读助手 / AI 阅读 / PDF 解读 / 知识库

## 截图脚本（3 张，1280×800）

| # | 场景 | 左侧页面 | 面板内容 |
|---|---|---|---|
| 1 | 划词提问 | 掘金技术文章快照 | 划词 chip + 提问 + 结构化回答（要点+代码高亮） |
| 2 | 多轮追问与图表 | 知乎/文章快照 | 两轮问答，回答含表格与 mermaid 图 |
| 3 | 知识保存 | 文章页 | 保存知识成功的状态（成功提示 + 知识卡片） |

## Promo 瓷砖（440×280）

- 主题句：`划词即问，边读边懂`
- 英文：`Ask as you read`
- 视觉：品牌蓝底 + 应用图标 + 主题句；由截图 spec 渲染产出（880×560@2x 缩至 440×280）

## 版本说明模板（What's new）

```
v0.1.0：首个公开版本。划词提问、整页解读、PDF 解读、多轮追问、图片提问、知识保存。
```

## 开发者后台填写清单

- [ ] 名称 / 简短描述 / 详细描述（本文案）
- [ ] 隐私政策 URL（托管 docs/store/privacy-policy.html）
- [ ] 联系邮箱：584839656@qq.com（已填入隐私政策页；开发者后台联系方式保持一致）
- [ ] 分类 Productivity
- [ ] 单一用途声明（permission-justification.md Single Purpose 段）
- [ ] 各权限 justification（permission-justification.md）
- [ ] 3 张截图 + promo 瓷砖（由 store-shots spec 产出）
- [ ] 语言：zh-CN（主）+ en（次）
