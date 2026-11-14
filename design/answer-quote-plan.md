# 回答划词追问（Answer Quote）方案

状态：**待评估**（未实施）。本文档回答"在 sidepanel 的 AI 回答上划词并追问"如何落地，供评审。

对照参考：Trae 对话的划词引用（本方案设计过程中实测过其负载形态，见 §8 决策记录）。

## 1. 背景与目标

当前追问只能凭会话记忆泛泛地问（材料重申机制重申的是**首轮划词材料**）。用户想针对
"回答里某个术语/某段结论"深挖时，需要手动复制粘贴。本方案补上这个缺口：

> 在回答气泡内选中任意文本 → 出现引用入口 → 片段以 chip 挂到提问框 → 输入问题发送 →
> 模型看到 `<answer_quote>` 锚定的片段并作答。

## 2. 现状链路（方案依赖的三条既有轨道）

### 2.1 会话历史的分层结构（为什么裸片段可行）

每次 `/api/ask` 请求，SDK 按轮次**全量重放**会话历史进 prompt；长会话超过
`Summary.EventThreshold/TokenThreshold` 后，老轮次折叠为摘要卡（cutoff 之前的不再重放），
细节经 recall_history 工具按需回溯（见 `internal/chat/agent.go` 摘要装配段）。

```
┌─ prompt 窗口 ─────────────────────────────┐
│ 老轮次：摘要卡（原文折叠，recall 可回溯）      │ ← 阈值之外
│ 近期轮次：全量重放（含上一条回答原文）          │ ← 阈值之内
│ 本轮：材料重申 + 问题                        │
└──────────────────────────────────────────┘
```

**推论**：划词追问锚定的对象几乎总是最近一条回答 → 处于全量重放区 → 片段裸传即可，
模型能看到上下文。唯一失效场景：**翻很久之前的回答划词**（该轮可能已被摘要折叠）→
由 §4 的 `blockContext` 兜底。

### 2.2 ask 请求形状（`sidepanel/js/net.js`）

```js
payload = { question, sessionId, images, context, page }
context = quotes.length ? { text, paragraph, pageContext, url, pageMd }   // 网页划词
        : (page?.pageMd ? { text:'', ..., url, pageMd } : null)          // 无划词页面采集
```

Go 侧 `handler.go`：`body.Context` → `AskContext{SelectedText, Paragraph, PageContext, URL, PageMd}`；
`SelectedText` 非空 = 首轮新划词；为空 = 追问轮 → 触发材料重申
（`sessionSvc.RecentReferenceMaterial` 查最近 `<reference_material>` 注入本轮 user message）。

### 2.3 quote-chip 组件（交互可直接复用）

`render.js` 已有划词标签机制：chip 节点带 `data-qid` 关联 `session.quotes` 完整数据，
挂到提问框上方。回答引用 chip 可复用同一视觉与挂载形态（数据源不同，见 §4）。

## 3. 交互设计

1. `messages` 容器上监听 `mouseup`：选区非空 **且** 选区落在 assistant 气泡内 →
   在选区附近浮出「引用追问」小按钮（Trae 同款形态；面板窄，不用右键菜单）
2. 点击按钮 → 片段挂到提问框上方（answer-quote chip，样式复用 quote-chip，加类型标识区分），
   输入框聚焦，用户输入问题
3. chip 可点击移除（同 quote-chip 交互）；再次划词替换旧 chip（v1 单条，见 §6）
4. 发送 → 走既有 ask 流程，payload 多一个 `answerQuote` 字段

与网页划词的互斥语义（状态机不交叉）：

| 入口 | 语义 | 会话影响 |
|---|---|---|
| 网页划词（quote-queue） | 新引用/新会话 | 现有逻辑，不动 |
| 回答内划词 | **当前会话追问** | 不进 quotes 队列，不产生新 chip 流 |

## 4. 数据流与字段设计

### 前端（sidepanel）

```js
// conversation.js 采集（自家渲染态 DOM，无注入成本）
function collectAnswerQuote(sel) {
  const bubble = /* sel.anchorNode 向上找 .msg 的 assistant 气泡 */;
  if (!bubble) return null;
  const text = sel.toString().trim();
  if (!text) return null;
  return {
    text,                                    // 选区原文（渲染态纯文本，v1 不反查源 markdown）
    blockContext: blockTextOf(sel),          // 选区所在块（p/li/pre/td）全文，截断 ~600 字符
    messageId: bubble.dataset.mid || ''      // 选区所属消息（前端定位/调试用，后端 v1 不依赖）
  };
}
```

```js
// net.js payload 扩展
payload.answerQuote = answerQuote;   // 独立字段，不拼 question（Trae 同款）
```

### Go（internal/chat）

```go
// handler.go：askReq 增加字段
type askQuote struct {
    Text         string `json:"text"`
    BlockContext string `json:"blockContext"`
}
// body.AnswerQuote → askCtx.AnswerQuote

// prompt.go：AskContext 增加 AnswerQuote
```

**注入规则**（buildUserMessageBase）：

```
追问分支（SelectedText 为空）：
  <answer_quote>            ← 新增，排在 reference_material 之前（本轮锚点优先）
    来源：本次回答 · 片段
    [text]
    所在段落：[blockContext]
  </answer_quote>
  <reference_material>…    ← 追问重申照旧（scope 内最近划词材料）
```

- **守卫**：`SelectedText` 非空（本轮是新网页划词）时不注入 answer_quote——两种引用互斥，
  后端按"新划词优先"静默丢弃 answerQuote（前端同时只挂一种 chip，正常到不了这里）
- **首轮分支/无材料分支**：不可能出现 answerQuote（没有回答就没有划词来源），无需处理
- **scope 语义**：answerQuote 轮 = 追问轮（SelectedText 为空）→ scope 继承、材料重申、
  pageContentBlock/pageURLBlock 守卫全部照常，零改动

### 既有块共存矩阵（prompt.go 一次理清）

| 块 | 首轮（新划词） | 追问（含回答划词） | 无材料 |
|---|---|---|---|
| page_context / page_url / reference_material | 现状 | 现状（重申） | 现状 |
| **answer_quote** | 不出现 | **SelectedText 空且 AnswerQuote 非空时注入** | 不出现 |

## 5. 风险与边界

| 风险 | 处理 | 理由 |
|---|---|---|
| 长会话翻旧回答划词（该轮已摘要折叠） | `blockContext` 兜底：片段 + 所在段随块注入 | 廉价保险；不带整条回答全文（未折叠时上下文本就有，重复注入反而膨胀） |
| 代码块/表格内划词：`selection.toString()` 跨块拍平结构 | v1 接受纯文本拍平（与网页划词同语义） | 后续增强：渲染 DOM 反查源 markdown 片段（render.js 保留块映射时） |
| mermaid/highlight 渲染产物混入 UI 字符 | blockTextOf 取文本节点拼接，跳过装饰节点 | 与 grab.js 的块级取文本同思路 |
| 选区跨两个回答气泡 | 只取锚点所在气泡的片段（与网页划词"选区容器"逻辑同思路） | 跨气泡引用语义模糊，不值得支持 |
| prompt 注入面 | answerQuote 是纯文本，与 SelectedText 同等待遇（无 HTML 执行面） | 无新增风险 |
| 窄面板浮层定位 | 按钮 anchor 到选区矩形右下，越界翻转 | 纯 UI 细节 |

## 6. 范围裁剪（v1 不做）

- 多条回答同时引用（chip 单条；多引用需求出现后再扩展为 [[$n]] 编号形态，net.js 已有
  多引用拼接的成熟先例）
- 反查源 markdown（保留表格/代码结构）
- 跨会话引用、引用历史回答之外的 UI 元素

## 7. 测试计划

**Go 单测**（prompt_test.go / handler_test.go）：
1. 追问 + answerQuote → user message 含 `<answer_quote>` 片段与所在段落、`<reference_material>` 重申并存
2. 首轮（SelectedText 非空）+ answerQuote → answer_quote 被守卫丢弃
3. answerQuote 空/空白 → 块不注入（现有用例不回归）
4. askReq 解码：AnswerQuote 字段映射

**前端**：
- collectAnswerQuote：jsdom 构造 .msg 气泡 + 选区，断言 text/blockContext/messageId 提取
- 手工验证清单：气泡内划词出按钮 / chip 挂载与移除 / 追问回答质量 / 摘要折叠场景（长会话造轮次后翻旧回答划词）/ 与网页划词互斥

## 8. 决策记录（评审对照）

| # | 决策点 | Trae 做法（实测负载 `{text, comment}`） | 本方案 | 差异原因 |
|---|---|---|---|---|
| 1 | 注入语义 | 片段独立字段，不拼问题；历史不重发（其模型上下文含全文） | **同**（独立字段 + 独立块） | 一致 |
| 2 | 邻近上下文 | **不带**裸片段 | **带所在块**（blockContext 兜底） | 我们有摘要折叠：翻旧回答时该轮原文可能不在场；且不带全文（在场时不重复注入） |
| 3 | 交互形态 | 选区 → 引用挂输入框 → 继续输入 | **同**，chip 组件复用 quote-chip | 现成组件 |

设计过程中修正过一次前提：最初误判"历史回答不在 prompt 里"（实际是全量重放 + 阈值摘要
折叠的分层结构，见 §2.1），因此决策 2 从"必须带上文"降级为"折叠区兜底"。

## 9. 实施步骤

1. 前端：collectAnswerQuote + 浮层按钮 + answer-quote chip（render.js / conversation.js，~150 行）
2. net.js payload 字段（~10 行）
3. Go：askReq/AskContext 字段 + prompt 注入块（handler.go / prompt.go，~80 行）
4. 测试：Go 4 用例 + 前端 1 套件 + 手工清单
5. 全量 `make test` → build/installer → 真机验证

预计量级与"方案 B（无划词采集）"相当或更小（无采集链改动、无缓存写入路径变化）。
