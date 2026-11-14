# 划词链路 HTML→MD 迁移方案（pageMd）

> 调研日期 2026-10-16。探针实证 + 全链路逐行梳理，结论均有代码位置与可复跑验证。

## 1. 背景与目标

划词提问时扩展采集整页 HTML（上限 300K 字符）传给桌面端，由 Go 端 `pageextract.go` 转为结构化文本存页面缓存。该链路存在三个问题：

1. **存储配额压力**：pageHtml 随会话持久化在 chrome.storage（5MB），已有两处"配额溢出剥 pageHtml 重试"补丁（`quote-queue.js:46`、`state.js:52`）
2. **隐私面过宽**：HTML 含 hidden input、内联脚本、注释、框架状态 JSON，其中可能嵌凭据；实际消费方（本地检索）只用可见文本
3. **token 效率**：若未来直接喂模型，脏 HTML 浪费窗口

**目标**：转换前置到扩展端（HTML → 朴素 MD），传输与存储只含可见内容，Go 端检索逻辑零改动。

## 2. 现状链路（逐行梳理结论）

```
grab.js(页面隔离世界)                     quote-collector.js(右键)        state.js                  conversation.js         net.js                    handler.go
{ text, paragraph, pageContext,    →  appendPendingQuote       →  游标消费入会话 quotes  →  refQuotes 快照      →  context 取第一条     →  ExtractPageText(PageHtml)
  url, pageHtml(≤300K), isComplete }    chrome.storage 队列        chip 落框                  (pageHtml 随行)          payload.context          → SavePageText(scope 缓存)
                                                                                                              { text,paragraph,        → WithPageSearchState
                                                                                                                pageContext,url,          → expand_context/pagesearch
                                                                                                                pageHtml }
```

关键代码位置：

| 环节 | 位置 | 说明 |
|---|---|---|
| 采集出口 | `extension/grab.js:152-200` | `grabContextInPage()` 返回结构；L168-172 截断 300K |
| HTML 来源 | `grab.js:71-133` | Defuddle 主内容 → 选择器链 → 选区容器 → body 兜底，四级降级 |
| 入队 | `background/quote-queue.js:23-56` | L46 配额溢出剥 pageHtml 重试 |
| 消费 | `sidepanel/js/state.js:93-110` | L52 同上补丁；会话 quotes 含 pageHtml |
| 提交 | `sidepanel/js/conversation.js:65-112` | refQuotes 快照；`runConversationOn` 剥 pageHtml 只存展示 |
| payload | `sidepanel/js/net.js:117-140` | `context` 只取第一条 quote，含 pageHtml |
| 请求体 | `internal/chat/handler.go:115-130` | `askReq.Context.PageHtml` |
| 转换 | `internal/chat/handler.go:241-250` | `retriever.ExtractPageText` → `SavePageText`（scope 感知缓存） |
| 检索消费 | `tool/pageretriever/pagesearch.go` | 标题切分 `headingLevel`(`^#{1,6} `)、`bodySegments`（已感知围栏）、紧凑子串匹配 |
| 参考材料 | `internal/chat/prompt.go:169-213` | `<reference_material>` 等标签注入，与 pageText 无耦合 |

**梳理中发现的现存缺陷**（本方案顺手修复）：

- `grab.js:199`：向上遍历未找到块级元素的兜底 return **漏了 pageHtml 字段** → 该路径页面缓存缺失
- `pagesearch.go:77 ParseHeadings` 无围栏状态机（`bodySegments` 有）→ 代码块内 `# 注释` 会被误判为标题，干扰 `sectionFrom` 切分边界

## 3. 探针实证（已跑通）

探针资产位于 `%TEMP%\absorb-mdprobe\`（临时目录，不入库）：

1. **Node 探针** `probe.mjs`：jsdom 加载 `docs/defuddle-coverage` 5 个真实页面快照 → 注入 `vendor/defuddle.min.js`（真实 vendor 代码）→ 复现 grab.js 第 0 级提取 → 转换器原型输出
2. **转换器原型** `convert.mjs`：`pageextract.go` 的 `renderNode`/`compactSpaces`/`collapseBlankLines` 逐行等价 JS 移植（ASCII 空白折叠语义严格对齐）
3. **Go 基线** `goprobe/main.go`：同一批 Defuddle 产物跑现有 `ExtractPageText`
4. **对比** `compare.mjs`：空白归一化后逐字符 diff

| 页面 | Defuddle 产物 | MD 输出 | 压缩率 | JS vs Go 同构 |
|---|---|---|---|---|
| github | 18,262 | 13,018 | 29% | **PASS** |
| wikipedia | 35,884 | 10,777 | 70% | **PASS** |
| mdn | 10,634 | 2,748 | 74% | **PASS** |
| blog | 10,552 | 5,821 | 45% | **PASS** |
| hackernews | 2,135 | 602 | 72% | **PASS** |

**结论：扩展端转换的产出与 Go 端现状输出逐字符等价（5/5），pagesearch/expand_context 零感知；对比整页快照（421K→13K 等）压缩 93%~99%。**

## 4. 改造方案

### Step 1 — 扩展端新增 `sidepanel/js/pageconvert.js`

**转换器选型**（结论：手写，不引第三方库）：

| 候选 | 体积 | 表格 | 与检索约定的适配成本 | 评估 |
|---|---|---|---|---|
| Turndown（事实标准） | ~50KB | 核心不支持，需 GFM 插件 | 链接/强调/表格的内联符号（`[]()`、`**`、`\|`）打断 pagesearch 紧凑匹配，需覆写 link 规则、去 GFM 插件——覆写后剩余核心即本转换器 | 多 50KB 依赖 + 一层符号噪声，收益为零 |
| Turndown + GFM 插件 | ~60KB | 管道表 | 同上，表格符仍打断匹配 | 同上 |
| node-html-markdown | ~30KB | 原生支持 | 同样的内联符号问题 | 维护活跃度一般 |
| rehype-remark（AST） | 大（unified 生态整包） | 管道表 | 最"正确"但对本场景过重 | 浏览器端引 unified 全家桶不合理 |
| **手写（探针 convert.mjs 转正）** | 0（~120 行） | v2 可选管道表 | **逐字符同构，pagesearch 零改动** | 零依赖；损失清单与成熟库相同（MD 表达力上限），无额外损失 |

选型依据：本场景的目标是"检索友好、与 Go 端 `ExtractPageText` 约定同构"，不是"最完整的 MD 表示"。成熟库的优势（内联链接/强调/表格标记）在本场景是噪声；其信息损失主要来自 MD 语法表达力上限（任何库相同）。探针已证手写实现 5/5 逐字符同构。

以探针 `convert.mjs` 为种子正式化：

- 入口 `extractPageText(htmlSrc: string): string`，内部用页面原生 `DOMParser`（grab.js 运行在页面隔离世界，直接可用；vitest 里用 jsdom 同实现）
- 输出约定与现 Go 端完全一致：`#`×level 标题行、```` ``` ```` 围栏代码块、块级元素换行分隔、连续空行压一；首行 `页面标题: <title>` 仅在输入含 `<head><title>` 时出现——探针实证 Defuddle 产物为片段 HTML（无 head），**划词主路径常态无标题行**（页面标题由 `pageContext` 字段独立承载），兜底路径（body/整页级 HTML）才会带首行

### Step 2 — `grab.js` 出口改 MD

- `grabPageHtml` 四级降级逻辑不动（它决定"取哪块 DOM"），在 `grabContextInPage` L168 后追加一步：`pageHtml → pageconvert.extractPageText()`，结果放新字段 **`pageMd`**
- 上限 `MAX_PAGE_HTML_CHARS=300000` → `MAX_PAGE_TEXT_CHARS=100000`（Go 端 `pageTextMaxLen=30000` rune 截断仍兜底，100K 留足余量）
- 顺手修复 L199 兜底 return 漏 `pageMd` 的缺陷
- `pageHtml` 字段**暂保留但置空**（一个版本期），省传输

### Step 3 — 字段更名的完整波及面

`pageHtml` → `pageMd` 共波及 **6 个源文件 + 1 个测试文件**（grep 实测 27 处），改造时逐处过：

| 文件 | 波及点 |
|---|---|
| `extension/grab.js` | 采集逻辑（L168-178）+ 5 处来源日志文案 |
| `background/quote-queue.js` | 入队字段（L40）+ 剥离补丁（L46-49）+ 日志 |
| `background/quote-collector.js` | 初始 ctx（L48）+ DEBUG 日志（L65-69） |
| `sidepanel/js/state.js` | 剥离补丁（L50-55）+ quote 结构注释（L123） |
| `sidepanel/js/conversation.js` | 注释同步（L118"剥离 pageHtml"） |
| `sidepanel/js/net.js` | payload 字段（L123）+ 注释（L97） |
| `frontend/tests/ExtensionBackground.spec.ts` | **14 处存量断言**（剥离降级/正常入队/查重用例）全部随字段改名 |

两处"配额溢出剥离重试"补丁**保留但改为剥离 pageMd**（防御纵深，正常情况下转换后 10-60KB 不会再触发）。

顺手项：`handler.go:123` 注释"上限 500K"与 grab.js 实际 300K 不一致，统一为实际值。

### Step 4 — `net.js` payload

`context.pageHtml` → `context.pageMd`（net.js:123）；旧字段一个版本内不再发送

### Step 5 — Go 端兼容读取（`handler.go`）

```go
// askReq.Context 增加字段：
PageMd string `json:"pageMd"` // 整页结构化文本（新扩展端已转换，直接入缓存）

// handler.go:241 读取顺序：
pageText := ""
if body.Context != nil {
    switch {
    case strings.TrimSpace(body.Context.PageMd) != "":
        pageText = truncatePageText(body.Context.PageMd) // 新链路：免转换，但截断守卫不可省
    case strings.TrimSpace(body.Context.PageHtml) != "":
        pageText = retriever.ExtractPageText(body.Context.PageHtml) // 旧扩展兼容（内含截断）
    }
}
```

**截断守卫必须保留**：现有 `pageTextMaxLen=30000` rune 截断发生在 `ExtractPageText` 内部，pageMd 直通会绕过它——超长 payload 将直写 SQLite。新增包内 `truncatePageText`（与 ExtractPageText 同规格：超限 rune 截断 + "…（已截断）"尾标），两条路径守卫规格一致。

`pageextract.go` 保留（旧扩展兼容期），`pagesearch.go` 零改动。

### Step 6 — 渐进迁移与收尾

- 版本共存期：旧扩展发 pageHtml、新扩展发 pageMd，Go 端双读——无需强制同步升级
- 下一版本：删除 `PageHtml` 字段与 `pageextract.go`（或保留给 `fetch_page` 非标准站点兜底，二选一到时评估）
- 可选增强（独立小改动）：`ParseHeadings` 补围栏状态机（与 `bodySegments` 对齐），修伪标题干扰 section 切分的现存边角

## 5. 自测覆盖

### JS 端（vitest，环境已具备：jsdom 29）

新增 `frontend/tests/pageconvert.spec.ts`：

1. **Golden 回归**：`docs/defuddle-coverage` 5 个 fixture → jsdom + `vendor/defuddle.min.js` → 转换 → 与快照文件逐字符断言（快照由探针产物 `jsmd/*.txt` 固化入库 `frontend/tests/fixtures/page-md/`）——同时兼任 defuddle 升级回归（对齐 defuddle-coverage README 的验证流程）
2. **结构约定**：标题行匹配 `/^ {0,3}#{1,6} /`；代码围栏成对；首行 `页面标题:` 约定
3. **边界**：空输入/纯 script 页/畸形 HTML（不 panic、返回空串或可解析文本）；300K→100K 截断
4. **grab.js 集成**：`ExtensionBackground.spec.ts` 补一条"pageMd 字段随划词入队"断言

### Go 端

`internal/chat/handler_test.go` 补：

1. `pageMd` 优先：payload 同时带 pageMd + pageHtml → 缓存落 pageMd 内容（不跑 ExtractPageText）
2. **`pageHtml` 回退**：仅带 pageHtml → 现行为不变（存量用例已覆盖解析，补端到端缓存断言）
3. **截断守卫**：pageMd 超过 `pageTextMaxLen` → 缓存落截断文本（两条路径守卫规格一致）
4. 均不带 → 走 LoadPageText 追问路径（现有覆盖保持）

### 端到端冒烟（手动）

新扩展 + 新桌面端：GitHub/Wikipedia/博客三类页划词 → 提问 → 追问触发 `expand_context` → 验证关键词命中章节与正文（对照旧版行为）；chrome://storage 查看会话体积对比。

### 基线命令

```bash
go test ./tool/... ./internal/chat/   # Go 基线（当前全绿）
cd frontend && npm test               # vitest 含新增 pageconvert.spec.ts
```

## 6. 影响面

| 维度 | 影响 |
|---|---|
| 协议 | 向后兼容：Go 双读 pageMd/pageHtml，新旧扩展共存；回滚 = 扩展端 pageMd 置空 |
| 存储 | 会话内 pageHtml（≤300K/条）→ pageMd（10-60K/条），5MB 配额压力实质消失 |
| 隐私 | HTML 隐藏内容（内联脚本/hidden 字段）不再出页面；**凭据泄露面归零**；数据安全问卷"页面内容"描述更窄更实 |
| 检索 | 零影响（探针 5/5 同构）；`pageTextMaxLen=30000` 内有效信息密度提升 |
| prompt | 零改动（pageContext/reference_material 链路与 pageText 无耦合） |
| 性能 | 扩展端转换 ~毫秒级（树遍历）；省 Go 端解析；网络 payload 减 60-97% |
| 代码 | 净减：两处剥离补丁保留但更名；pageextract.go 进入退役倒计时 |

## 7. 风险与缓解

| 风险 | 缓解 |
|---|---|
| 浏览器 DOMParser 与 Go html.Parse 对畸形 HTML 解析差异 | 探针已证真实页面 5/5 一致；消费端（pagesearch）空白/标记不敏感，语义等价即可；Go 兜底 `ExtractPageText` 保留一个版本 |
| 某站点 Defuddle 产物异常 → 转换空 | grab.js 四级降级不动，转换前判空回退原 HTML 直传（pageMd 空 + pageHtml 兜底） |
| 截断点从 300K 降为 100K 丢长文尾部 | Go 端 30000 rune 截断本就生效，扩展端 100K ≥ 3× 终态上限，无实质损失 |
| 旧扩展发 pageHtml 的存量会话缓存 | 缓存按 scope 存新读新写，格式自描述（都是同构文本），无迁移需求 |
