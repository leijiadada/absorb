# Absorb Visual DSL v1 规范

LLM 输出 `:::absorb-visual` 包裹的 Markdown+DSL，扩展侧边栏将其渲染为可视化页面。
本文是**契约文档**：解析器（parser.js）、渲染器（renderer.js）、分段编排与失败态（blocks.js）、
样式（visual.css）与 `skills/visual-explanation/SKILL.md` 共同遵守，任何一方改动需同步更新。

## 设计原则

1. **LLM 只组织内容与结构，Absorb 负责布局、样式与图表绘制**。LLM 不输出 HTML/CSS/SVG 坐标。
2. **正文走 Markdown，组件源格式是声明式行**：容器内的正文与 callout 体内是 Markdown，
   由既有 marked + DOMPurify 管线渲染；kv / timeline / tree / chart 四种组件的**源格式**
   是声明式行（`key :: value`、`- 标签：描述`、缩进树、`名称 :: 数字`），不是 Markdown。
   这些组件的 value / 文本在渲染时会再走 Markdown（支持行内代码与加粗），但块级结构
   （列表、表格、代码块）不会在组件体内生效。chart 的数据契约借鉴 fixture.json：
   LLM 只填数据，不写坐标/颜色/样式，由渲染器按最大值归一生成条形。
3. **失败降级**：容器缺失 → 纯 Markdown 回答；组件畸形 → 退化为普通 Markdown 块；任何渲染异常
   → 整块按原始 Markdown 渲染。可视化永不吞掉回答内容，永不阻塞消息流。
4. **流式不渲染**：流式阶段可视化块只显示占位提示，回答结束（终帧/历史重建）后才解析渲染。
5. **安全**：所有 LLM 文本经 DOMPurify 或 textContent 落地；mermaid 走既有 strict 模式管线。

## 外层容器

    :::absorb-visual
    ---
    template: sheet        # sheet（面板卡片，默认）| doc（连续文档）
    title: 标题            # 可选
    subtitle: 副标题       # 可选
    ---

    （前导区：第一个 ## 标题之前的内容，渲染为导语）

    ## 第一节标题

    正文 Markdown……

    ## 第二节标题

    ……

    :::

闭合规则（**就近闭合**）：容器只在**顶层**的裸冒号行处闭合（冒号数与开行相等）；
容器体内一旦有组件开行，后续第一个裸冒号行先闭合该组件，不闭合容器。

- 这条规则是线上故障的修复结果：早先「容器只看冒号数」导致组件闭合行把容器提前关掉，
  正文外溢到容器外、末尾留下孤儿 `:::`。
- 围栏组件的闭合由围栏自身承担（`` ``` `` 同字符、长度不小于开行），不参与冒号判定。
- 围栏组件**未闭合**（漏写闭合围栏）时，fence 状态会一直开到文末，容器随之不闭合，
  容器**之后**的回答内容被一并收进容器内。此时 errors 报的「容器未闭合」只是表象，
  真因是围栏没配对——排查时先数围栏。
- 到文末未闭合（如回答被截断）：仍宽容渲染已收到的部分，并向 errors 收集一条告警。
  该告警带 `repaired: true` 标记——语义为「已按文末自动闭合，渲染结果与显式闭合一致，
  非致命」；统计语法纪律时据此与致命错误区分。

隐式容器（唯一例外，2026-11 线上故障后加的兜底）：整篇回答**没有任何** `:::absorb-visual`
容器、却出现了组件围栏（`tree` / `kv` / `chart` 等）时，解析器把每个组件块各自包成一个
独立可视化段渲染，其余内容仍按普通 Markdown，顺序不变；同时向 errors 记一条「缺少容器」。
- 只在**全文无容器**时启用：已有容器说明格式正确，不再猜测，避免把正文里作为示例的
  ` ```tree ` 代码块误提升。
- 触发时组件落在导语区（合成的容器没有 `##` 面板），视觉上仍是完整组件，不是代码块。

输入归一化：解析前剥除 BOM、`\r\n`/`\r` 统一为 `\n`（LLM 或剪贴板来源的换行差异不影响解析）。

frontmatter 自愈（2026-11 线上泄漏修复）：`---` 不成对（漏写闭合行）或整体缺失时，
容器开行后**紧邻的键行串**（仅 `template` / `title` / `subtitle` / `version`，允许中间空行）
仍识别为元信息，不再漏成可见正文；正文从首个非键行开始。键行串为空时原文返回，
正文恰好以 `title: ` 开头的罕见形状不受影响。`---` 成对时行为不变（闭合行内全量解析，
未知键忽略）。

表格自愈：容器内的 Markdown、以及 **callout 组件体内的 Markdown**（五个组件里唯一以
Markdown 为体的），若表格缺少 `| --- |` 分隔行（表头行后紧跟数据行），解析器自动补一行
分隔行——这是 LLM 输出表格的高频失误，marked 无分隔行不识别为表格。
**代码块围栏（`js`、`python` 等非组件语言）内不做修复**；kv / timeline 的值不跑自愈（它们不是
Markdown 块，块级结构在其中不生效）。

## 组件

组件有**两种等价写法**（与上游 answer-me-with-html 一致）：

- **围栏写法（推荐）**：`` ```name args `` 开、`` ``` `` 闭。与 mermaid、代码块同一语法直觉，
  模型最不容易写错（没有冒号计数负担）。
  体内要放代码块时，外层围栏必须改用 4 个反引号（开行四个反引号 + 组件名，闭合行也是四个），
  否则体内的三反引号代码围栏会被当成组件闭合行——这是 CommonMark 语义（闭合行只认围栏字符，
  不认语言名）。冒号写法没有此限制。
- **冒号写法（兼容）**：`:::name args` 开、裸冒号行闭。

两种写法**按组件名识别**，不靠冒号数量区分层级：`:::chart` 与 `::::chart` 都认；
任意裸冒号行（≥3 个冒号）都闭合当前组件。容器名 `absorb-visual` 不在组件白名单。

以下示例一律用围栏写法；把开行围栏换成 `:::`（或 `::::`）、闭合围栏换成 `:::` 即等价冒号写法。

表格对比不需要组件——容器内的 Markdown 表格自动获得对比样式。

### callout — 提示框

    ```callout tip
    **要点**：跳表的核心是空间换时间。
    ```

variant：`tip`（默认 `info`）｜`warn`。体内为 Markdown。

### kv — 键值对网格

    ```kv
    平均复杂度 :: O(log n)
    实现方式 :: 有序链表 + 多级索引
    ```

每行 `key :: value`；无分隔符的续行并入上一条 value。渲染为 dl/dt/dd 网格。

### timeline — 时间线/步骤序列

    ```timeline
    - SYN：客户端发送同步报文，进入 SYN_SENT
    - SYN+ACK：服务端确认并反向同步，进入 SYN_RCVD
    ```

每行一个条目 `- 标签：描述`（首个"："/": "分隔；缺标签则整行为描述）；缩进续行并入描述。
渲染为带节点圆点的有序序列（ol）。

### tree — 层级树

    ```tree
    - 跳表
      - Level 2 索引
      - Level 1 索引
    - 哈希表
    ```

2 空格缩进 = 一层。渲染为带连接线的嵌套列表（ul）。

### chart — 数量对比条形图（V1 仅 bar，纯 CSS）

    ```chart bar
    title: 构建耗时对比        # 可选元信息，仅在数据行前，单冒号
    unit: 秒                  # 可选
    Go :: 4.2                 # 数据行：名称 :: 纯数字
    Rust :: 1.1
    Java :: 12.8
    ```

- 开行 `chart bar`（类型参数，缺省 bar；其余类型整块退化为原文并记 errors）。
- 数据行 `名称 :: 数字`（与 kv 同款 ` :: ` 分隔）；数值须可解析为有限非负数。
- 渲染为横向条形（窄面板友好）：条宽 = 值/最大值，正值条宽下限 2% 保证可见；
  label 与数值走 textContent，深色主题跟随 `--av-*` token。
- 降级纪律：单行畸形（缺分隔符/数值无效/负数）→ 丢弃该行并记 errors，其余照常渲染；
  **零条有效数据** → 整块退化为原文（markdown），不渲染空图。

### mermaid — 图表

直接在容器内写 ```mermaid 代码块（只用 flowchart / sequenceDiagram /
stateDiagram-v2 / classDiagram / erDiagram），由既有 Mermaid 管线渲染；
语法非法时自动保留为代码块。语言名不是组件名（也不在 mermaid 名单）的围栏
（`js`、`python` 等）一律按普通代码块渲染。

## 触发规则（给 LLM 的白名单）

| 场景 | 是否可视化 |
| --- | --- |
| 术语解释、单点疑问、简单问答 | 否 |
| ≥3 个关联概念构成的结构/机制 | 是 |
| 多步骤流程（协议握手、请求链路、生命周期） | 是 |
| 交互时序、状态机 | 是 |
| 多维度对比（≥3 维或 ≥3 对象） | 是 |
| 数量对比（耗时/占比/指标等带数字的比较） | 是（`chart bar`） |
| 用户明确要求画图/图示化 | 是 |

## 解析器实现要点

- 分段：`parseVisualSegments(content, errors)` → `[{type:'md'|'visual', ...}]`，全程围栏感知。
- 文档：`parseVisualDocument(raw, errors)` → `{meta, preamble, sections:[{title, blocks}]}`，
  blocks 为 `{kind:'md'|'callout'|'kv'|'timeline'|'tree'|'chart', ...}`。
- 错误收集（借鉴 answer-me-with-html）：**全程收集而非 fail-fast**。`errors` 是可选数组参数，
  解析器把非致命问题以 `{line, component, message}` 追加进去，畸形部分照常退化为 markdown，
  渲染不中断。**行号基准按函数区分**：`parseVisualSegments` 的「容器未闭合」用**全文** 1-based
  行号；`parseVisualDocument` 及其组件错误用 **raw 内** 1-based 行号（含容器开行与 frontmatter）。
  component 为 `absorb-visual` 或**任意组件名**（含未知名，如 `note`、`mindmap`）。
  胶水层（render.js）在终帧若 errors 非空则 `console.debug` 留痕，
  渲染异常时降级为带原因提示的原始 Markdown 框（`.absorb-visual-degraded`）。
  已收集的问题类型：容器未闭合、未知组件名（含 `:::note` 这类非白名单写法，整块保持原文但
  其闭合行不会误关容器）、tree 出现 Mermaid 习惯写法（`A[开始]`）、
  kv 首条缺 ` :: ` 分隔符的残句、chart 的畸形行（缺分隔符/数值无效/负数，逐行丢弃留痕）、
  chart 无有效数据或类型不支持（整块退化留痕）、缺少 `:::absorb-visual` 容器
  （组件按隐式容器渲染，见「外层容器」）。
- 零依赖：parser.js 不引用 DOM 与任何 Absorb 模块，可独立单测、可整体拷贝复用。
- 分段编排（blocks.js）：把回答切分为 md / visual 并装配节点；`final=false` 时 visual 段
  一律占位（不解析），`final=true` 时交渲染器，抛异常则记录原因并降级为原始 Markdown。
  依赖注入 `el / mdToHtml / text`，同样零 Absorb 依赖、可独立单测；胶水层 render.js
  只做依赖绑定与 DocumentFragment 装配。
- 隐式容器（parser.js 内 `parseImplicitComponents`）：全文无容器时把顶层组件围栏块包成
  独立 visual 段（合成容器头 + 原文 + 闭合行），已收集的问题类型里对应「缺少容器」告警。
