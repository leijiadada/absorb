# page-corpus/ — 划词链路页面语料库

pageconvert（转换）与 pagesearch（检索）的**覆盖面回归语料**：真实页面快照 + 每页断言。
消费方：`frontend/tests/pageconvert.spec.ts`（转换回归）与 `tool/pageretriever/corpus_test.go`
（检索评估基准 + JS/Go 同构复验）。语料随仓库入库（CI 回归依赖）；例外：`spa-modal-noise/`
为内网管理后台实抓快照，仅存本地不入库（公开仓库不含内部信息），缺失时相关用例自动跳过。

## 语料构成（18 页）

| 类型 | 页面 | 来源 | 验证点 |
|---|---|---|---|
| blog | medium | readability@0.4.4 medium-1 | 博客平台 |
| blog-cn | cnblogs / segmentfault | 2026-10-16 实抓 | 中文博客/问答，评论区噪音 |
| community | lwn / v2ex / zhihu | readability@0.4.4 / 浏览器实抓 / 浏览器实抓（登录态） | 社区长文 / 中文论坛帖 / **知乎问答（登录墙内容）** |
| docs | google-sre-book / ietf / yuque | readability@0.4.4 / 浏览器实抓 | 书籍章节 / RFC / **语雀文档（CSR 渲染，665KB→5.8K）** |
| media-embeds | embedded-videos | readability@0.4.4 | 视频嵌入噪音页 |
| news | nytimes / gmw | readability@0.4.4 / 实抓 | 英文新闻 / 中文新闻（光明网） |
| spa-shell | source.html | 本地构造 | **CSR 壳页边界基线**：转换产出极薄 → 缓存缺失 → 检索无缓存，主问答不受影响 |
| spa-modal-noise | source.html | 内网后台实抓（581KB，平台信息脱敏）**（不入库，仅本地）** | **SPA 关闭态弹窗噪音页**：通知弹窗×12 份副本常驻 DOM（`display:none`），raw 转换 30007 触顶；html-pruner 剪枝后 991 字符 |
| table-heavy | keep-tabular-data | readability@0.4.4 | 表格密集页 |
| tech-community | juejin | 浏览器实抓（掘金文章，437KB） | 中文技术社区 SPA-SSR 混合渲染 |
| tech-media | cnet / qq | readability@0.4.4 | 科技媒体 / 中文门户（腾讯网，320KB 大页面） |
| weixin-gzh | 武昌工学院公众号文章 | 浏览器实抓（4.7MB HTML） | **公众号形态**：巨型 HTML、内联样式极重，正文占比极低 |

抓取通道说明：curl 可达的站直接抓；登录墙/动态站（知乎、语雀、掘金、公众号）经浏览器控制
（TRAE Chrome Extension）用用户登录态打开真实页面，fetch POST 到本地接收服务
（127.0.0.1:18923，按 name 直写语料目录）。

## meta.json 约定

每页一份，字段：
- `expect.nonEmpty`：常规页 true；壳页 false（断言转换产出 ≤100 字符）
- `expect.mdChars`：**入缓存最终值**（扩展端转换产出经 Go `TruncatePageText` 同规格截断后的 rune 数）——Go 端 corpus 测试用同一 HTML 重算比对，持续复验 JS/Go 逐字符同构
- `expect.mustContain`：转换文本中必须存在的词（紧凑匹配）
- `queries[]`：检索评估用——`keywords` + `expectHit`（true=正文/标题词必命中，false=编造词零命中）

meta 由脚本按转换产出自动生成 + 人工复核（正文词偏导航词的已换成标题主题词）。

**口径注意**：`expect.mdChars` 是转换器（pageconvert.js / Go ExtractPageText）对 `source.html`
的 **raw 基线**——生产 pageMd 在转换前还经 `extension/html-pruner.js` 剪掉不可见 DOM
（关闭态弹窗/隐藏元素），实际入缓存的值 ≤ mdChars。剪枝行为由
`frontend/tests/extension/htmlpruner.spec.ts` 单测 + 集成断言覆盖，不进入本语料同构比对。

## 维护

- 新增语料：`<type>/<page>/source.html` + 跑 meta 生成流程 + 复核 queries
-mozilla 来源页可用 jsDelivr 拉取（本机直连 GitHub 不通时）：
  `https://fastly.jsdelivr.net/gh/mozilla/readability@0.4.4/test/test-pages/<name>/source.html`
- 抓中文站：列表页提取文章链接 → 抓文章页；抓不到的站（知乎/公众号登录墙）不硬抓，
  其行为由 spa-shell 边界样本代表
