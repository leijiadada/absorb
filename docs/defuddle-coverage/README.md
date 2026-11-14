# coverage/ — Defuddle 覆盖率测试语料

`extension/sidepanel/vendor/defuddle.min.js` 升级时的回归验证快照，覆盖 5 类典型页面：

| 文件 | 类型 | 快照来源 |
|------|------|----------|
| `github.html` | 代码仓库页（GitHub extractor） | github.com/kepano/defuddle |
| `wikipedia.html` | 百科页（Wikipedia extractor） | zh.wikipedia.org 机器学习 |
| `mdn.html` | 文档站 | MDN Web/JavaScript |
| `blog.html` | 个人博客文章 | ruanyifeng.com curl 用法指南 |
| `hackernews.html` | 论坛讨论页（HN extractor） | news.ycombinator.com item 41549977 |

基线数据（v0.19.2，快照 53KB-411KB → 提取 0.6K-18.6K 字，5/5 正确识别主内容与标题）。

## 升级时验证

```bash
npx defuddle@<新版本> parse github.html
# 逐个检查：title 正确、正文完整、无导航/侧栏噪音；对比上表基线无回退
```

抓取时间：2026-10-04。快照是历史样本，仅验证"提取能力不回退"，不代表站点当前内容。
