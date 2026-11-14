# vendor/ — 第三方前端库

本目录存放从 CDN 下载、本地打包的第三方压缩库，用于 Absorb 浏览器插件侧边栏渲染。

## 为什么本地打包

MV3 扩展存在严格 CSP，默认**禁止加载远程脚本**（`default-src 'self'; script-src 'self'`），
因此 marked / DOMPurify / highlight.js 必须随扩展一起打包，不能外链 CDN。

## 使用约定

- 这些文件**不属于业务代码**，不要直接修改。
- 升级时整文件替换，并**同步更新下方「来源与版本」表的 URL/版本**。
- 业务侧引用方式和顺序见 `sidepanel.html`；其中 marked / DOMPurify / highlight.js 以经典
  `<script>` 方式加载为全局变量（`window.marked` / `window.DOMPurify` / `window.hljs`），
  业务源码（`js/`、ES Module）通过 `render.js` 使用这些全局变量，不做 npm import。

## 来源与版本

| 文件 | 用途 | 版本 | 来源 |
|------|------|------|------|
| `marked.min.js` | Markdown 解析渲染 | v15.0.12 | https://cdnjs.cloudflare.com/ajax/libs/marked/15.0.12/marked.min.js |
| `purify.min.js` | XSS 净化（DOMPurify） | 3.4.14 | https://cdnjs.cloudflare.com/ajax/libs/dompurify/3.4.14/purify.min.js |
| `highlight.min.js` | 代码语法高亮 | v11.9.0 | https://unpkg.com/@highlightjs/cdn-assets@11.9.0/highlight.min.js |
| `highlight.github.min.css` | 高亮浅色主题（github） | 11.9.0 | https://cdnjs.cloudflare.com/ajax/libs/highlight.js/11.9.0/styles/github.min.css |
| `mermaid.min.js` | Mermaid 图表渲染 | v9.4.3 | https://cdn.jsdelivr.net/npm/mermaid@9.4.3/dist/mermaid.min.js |
| `defuddle.min.js` | 页面主内容提取（划词采集用） | 0.19.2 | https://registry.npmjs.org/defuddle/-/defuddle-0.19.2.tgz （dist/index.js） |

> `highlight.min.js` 为常见语言构建（内含约 36 种语言，见 `hljs.listLanguages()`），
> 覆盖 go / python / javascript / typescript / json / yaml / sql / bash / cpp / markdown 等常用语言。

> `mermaid.min.js` 特殊：**不在 `sidepanel.html` 静态加载**（体积 2.8MB，拖慢面板首开），
> 由 `js/render.js` 在首次遇到 mermaid 代码块时动态注入 `<script>` 懒加载。
> 选 v9 而非 v10+：v10 起改为 ESM + 动态 import，在 MV3 严格 CSP 下有兼容风险，v9 UMD 直接可用。

> `defuddle.min.js` 特殊：**不用于 sidepanel**，由 `background/quote-collector.js` 在划词手势时经
> `chrome.scripting.executeScript({ files: [...] })` 注入页面隔离世界（UMD 挂 `Defuddle` 全局），
> 供 `grab.js` 的 `grabPageHtml` 做主内容提取。选 core 版（dist/index.js，含全量站点
> extractor，运行时零依赖）；升级属 breaking 敏感操作，流程见仓库根 README 或升级语料说明。

## 引用顺序（sidepanel.html）

```html
<link rel="stylesheet" href="vendor/highlight.github.min.css" />
<script src="vendor/marked.min.js"></script>
<script src="vendor/purify.min.js"></script>
<script src="vendor/highlight.min.js"></script>
<script type="module" src="js/main.js"></script>
```

顺序要点：经典 `<script>` 在 `<script type="module">`（deferred）之前顺序执行，
保证 `render.js` 运行时 `marked` / `hljs` / `DOMPurify` 三个全局变量已就绪。