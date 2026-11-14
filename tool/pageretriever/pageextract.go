package retriever

import (
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// pageextract.go：HTML → 结构化文本（一次性抽取，供本地检索用）。
//
// 划词时前端把主内容区 HTML 随上下文发给 Go 端，
// 此处抽取为带结构标记的纯文本（markdown 风格），缓存在 SQLite（约 ≤30K 字符），
// expand_context 工具直接在缓存文本上检索，零浏览器往返。
//
// 结构标记约定（pagesearch.go 依赖）：
//   - 首行「页面标题: <title>」（title 存在时）
//   - H1-H6 →「#」×level 前缀行，如「### 3.1 Runner 的定义」
//   - PRE  → ``` 围栏代码块
//   - 其余块级元素文本 → 普通行，行间以空行分隔
//
// HTML 来自任意网页（不可信输入），golang.org/x/net/html 是 WHATWG 兼容解析器，
// 浏览器级容错，畸形 HTML 不会 panic。

const (
	// PageTextMaxLen 结构化文本上限（防爆 token）。导出供文档准备链路复用：
	// document_contents 状态判定（超出标 partial）与注入截断共用同一规格。
	PageTextMaxLen = 30000

	// ---------- 图片占位（与 JS pageconvert.js emitImage 同构） ----------
	// 网页图片在纯文本管线中原本被静默丢弃；产出 Markdown 图片占位行
	// ![alt|宽x高](src)，让模型感知图片的存在与主题（视觉识别走 images 数组）。
	imgMaxLines  = 20  // 单页占位行上限：防图片密集页挤占正文预算
	imgLabelMax  = 60  // 占位标签（alt）长度上限（rune）
	imgSrcMax    = 300 // 占位 URL 长度上限（rune）
	imgFileBackg = "图片"
)

// decorImgRe 装饰图启发式：无 alt 且路径命中图标/占位关键词 → 跳过。
// （与 JS DECOR_RE 同表：icon/logo/sprite/spacer）
var decorImgRe = regexp.MustCompile(`(?i)(icon|logo|sprite|spacer)`)

// imgState 单次渲染的图片占位状态（超 IMG_MAX_LINES 只计数，收尾补省略行）。
type imgState struct {
	emitted int
	omitted int
}

// attrStr 读元素的字符串属性（缺失返回空串，值 trim）。
func attrStr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return strings.TrimSpace(a.Val)
		}
	}
	return ""
}

// imgLabel 占位标签：alt → title → 文件名（src 末段）→「图片」；宽高属性齐全时
// 追加 |宽x高。label/src 含 ']' 或 ')' 时不转义——Markdown 严格解析会截断占位，
// LLM 宽容读取下可接受，转义收益低于实现复杂度（与 JS 同边界）。
func imgLabel(n *html.Node, src string) string {
	label := compactSpaces(attrStr(n, "alt"))
	if label == "" {
		label = compactSpaces(attrStr(n, "title"))
	}
	if label == "" {
		name := src
		if i := strings.IndexAny(name, "?#"); i >= 0 {
			name = name[:i]
		}
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		label = compactSpaces(name)
	}
	if label == "" {
		label = imgFileBackg
	}
	if r := []rune(label); len(r) > imgLabelMax {
		label = string(r[:imgLabelMax]) + "…"
	}
	w, h := attrInt(n, "width", 0), attrInt(n, "height", 0)
	if w > 0 && h > 0 {
		label += "|" + strconv.Itoa(w) + "x" + strconv.Itoa(h)
	}
	return label
}

// emitImage 输出单张图片占位。src 优先级 data-src → data-original → src（懒加载页
// 的 src 常是占位图如 loading.gif，真实地址在 data-src 属性中，随序列化 HTML 保留）；
// data-uri / javascript: 跳过（防超长与脚本向量）；无 alt 的装饰图跳过；超过
// imgMaxLines 只计数不输出（ExtractPageText 收尾补省略行）。
func emitImage(n *html.Node, b *strings.Builder, st *imgState) {
	if st.emitted >= imgMaxLines {
		st.omitted++
		return
	}
	src := attrStr(n, "data-src")
	if src == "" {
		src = attrStr(n, "data-original")
	}
	if src == "" {
		src = attrStr(n, "src")
	}
	if src == "" || strings.HasPrefix(src, "data:") || strings.HasPrefix(src, "javascript:") {
		return
	}
	alt := compactSpaces(attrStr(n, "alt"))
	if alt == "" && decorImgRe.MatchString(src) {
		return // 无 alt 的装饰图（图标/logo/雪碧图）
	}
	label := imgLabel(n, src)
	if r := []rune(src); len(r) > imgSrcMax {
		src = string(r[:imgSrcMax]) + "…"
	}
	st.emitted++
	b.WriteString("![" + label + "](" + src + ")")
}

// ExtractPageText 把 HTML 源码抽取为结构化文本。
// 生产链路的转换由扩展端 pageconvert.js 承担（pageMd 直读入缓存），本函数保留：
//  1. corpus 基准的同构交叉验证锚点（与 JS 实现互证，任一端行为漂移即失败）；
//  2. pagesearch_test / expand_test 的 fixture 转换；
//  3. Go 侧未来需要 HTML 转换时的现成实现。
func ExtractPageText(htmlSrc string) string {
	if len(strings.TrimSpace(htmlSrc)) == 0 {
		return ""
	}
	doc, err := html.Parse(strings.NewReader(htmlSrc))
	if err != nil {
		return ""
	}

	var b strings.Builder
	// title：<head><title> 文本（找不到则无首行）
	if t := findTitle(doc); t != "" {
		b.WriteString("页面标题: " + t + "\n\n")
	}
	// 从 body 开始 walk（body 不存在时用整个文档）
	body := findFirst(doc, "body")
	if body == nil {
		body = doc
	}
	st := &imgState{}
	renderNode(body, &b, st)
	// 超上限的图片统一补一行省略说明（与 JS extractPageText 收尾同构）
	if st.omitted > 0 {
		b.WriteString("\n（另省略 " + strconv.Itoa(st.omitted) + " 张图）\n")
	}

	out := collapseBlankLines(b.String())
	out = strings.TrimSpace(out)
	return TruncatePageText(out)
}

// TruncatePageText 结构化文本的统一截断守卫（PageTextMaxLen rune 上限）。
// ExtractPageText 内部复用；chat 包对扩展直传的 pageMd（免转换路径）应用同一规格，
// 保证两条缓存写入路径的长度守卫一致。
func TruncatePageText(s string) string {
	if runes := []rune(s); len(runes) > PageTextMaxLen {
		return string(runes[:PageTextMaxLen]) + "\n…（已截断）"
	}
	return s
}

// attrInt 读元素的整数属性（缺失/非法返回默认值）。
func attrInt(n *html.Node, key string, def int) int {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			if v, err := strconv.Atoi(strings.TrimSpace(a.Val)); err == nil {
				return v
			}
		}
	}
	return def
}

// renderPipeTable 尝试把 table 渲染为 Markdown 管道表格（行列关系保留）；
// 返回 false 表示判定为布局表格或无有效行列，调用方回退通用块级渲染（散行）。
// 规则（边界探针 32 用例 + 决策 A1-A4）：
//   - colspan 横向占位、rowspan 纵向复制填充（每行自洽，LLM 检索友好）
//   - 单元格内 | 直接移除（转义 \| 会引入反斜杠，破坏紧凑匹配一致性）
//   - caption 输出为表格前独立行；多级表头取首个 tr，其余行自然并入体部
//   - 布局表格（>50% 单元格内含 p/h1-h6 块级正文元素，借鉴 readability
//     _cleanConditionally 思路）回退散行，避免排版垃圾变成管道表
//   - 嵌套 table：内层文本并入外层单元格，不递归管道化；tr 外的游离文本忽略
func renderPipeTable(table *html.Node, b *strings.Builder) bool {
	var trs []*html.Node
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}
		if n != table && strings.EqualFold(n.Data, "table") {
			return // 嵌套 table：其文本经外层单元格 inlineText 保留
		}
		if strings.EqualFold(n.Data, "tr") {
			trs = append(trs, n)
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(table)
	if len(trs) == 0 {
		return false
	}

	cellText := func(n *html.Node) string {
		t := strings.ReplaceAll(inlineText(n), "|", "")
		return strings.TrimSpace(t)
	}
	hasBlockChild := func(n *html.Node) bool {
		var found bool
		var scan func(x *html.Node)
		scan = func(x *html.Node) {
			if found || x.Type != html.ElementNode {
				if found {
					return
				}
			}
			if x != n && x.Type == html.ElementNode {
				switch x.Data {
				case "p", "h1", "h2", "h3", "h4", "h5", "h6":
					found = true
					return
				}
			}
			for c := x.FirstChild; c != nil && !found; c = c.NextSibling {
				scan(c)
			}
		}
		scan(n)
		return found
	}

	// 布局表格判定：>50% 单元格内含块级正文元素 → 回退散行
	blocky, total := 0, 0
	for _, tr := range trs {
		for c := tr.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode && (strings.EqualFold(c.Data, "td") || strings.EqualFold(c.Data, "th")) {
				total++
				if hasBlockChild(c) {
					blocky++
				}
			}
		}
	}
	if total > 0 && blocky*2 > total {
		return false
	}

	// 网格填充：colspan 横向占位，rowspan 纵向复制填充
	var grid [][]string
	for ri, tr := range trs {
		col := 0
		for c := tr.FirstChild; c != nil; c = c.NextSibling {
			if c.Type != html.ElementNode {
				continue
			}
			if !strings.EqualFold(c.Data, "td") && !strings.EqualFold(c.Data, "th") {
				continue
			}
			for ri < len(grid) && col < len(grid[ri]) && grid[ri][col] != "" {
				col++ // 跳过被上方 rowspan 占用的列
			}
			cs := attrInt(c, "colspan", 1)
			rs := attrInt(c, "rowspan", 1)
			if cs < 1 {
				cs = 1
			}
			if rs < 1 {
				rs = 1
			}
			text := cellText(c)
			for dr := 0; dr < rs; dr++ {
				for len(grid) <= ri+dr {
					grid = append(grid, nil)
				}
				for dc := 0; dc < cs; dc++ {
					for len(grid[ri+dr]) <= col+dc {
						grid[ri+dr] = append(grid[ri+dr], "")
					}
					grid[ri+dr][col+dc] = text // 复制填充（A1b）
				}
			}
			col += cs
		}
	}
	width := 0
	for _, row := range grid {
		if len(row) > width {
			width = len(row)
		}
	}
	if width == 0 {
		return false
	}

	// caption（A2）：表格前独立行
	for c := table.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && strings.EqualFold(c.Data, "caption") {
			if capText := strings.TrimSpace(inlineText(c)); capText != "" {
				b.WriteString("\n" + capText + "\n\n")
			}
			break
		}
	}

	// 管道行输出：首个 tr 为表头行，其后固定分隔行
	b.WriteString("\n")
	for ri, row := range grid {
		cells := make([]string, width)
		for ci := 0; ci < width; ci++ {
			if ci < len(row) {
				cells[ci] = row[ci]
			}
		}
		b.WriteString("| " + strings.Join(cells, " | ") + " |\n")
		if ri == 0 {
			b.WriteString("|" + strings.Repeat(" --- |", width) + "\n")
		}
	}
	b.WriteString("\n")
	return true
}

// renderNode 深度优先渲染：块级元素前后换行，标题/代码块特殊格式。
// st 为图片占位状态（ExtractPageText 每次 call 新建，同步渲染无重入）。
func renderNode(n *html.Node, b *strings.Builder, st *imgState) {
	if n.Type != html.ElementNode {
		if n.Type == html.TextNode {
			b.WriteString(compactSpaces(n.Data))
		}
		return
	}
	if strings.EqualFold(n.Data, "img") {
		emitImage(n, b, st)
		return
	}
	// 表格优先走管道化（行列关系保留）；布局表格/畸形结构回退通用块级渲染
	if strings.EqualFold(n.Data, "table") {
		if renderPipeTable(n, b) {
			return
		}
		b.WriteString("\n")
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			renderNode(c, b, st)
		}
		b.WriteString("\n")
		return
	}
	switch n.Data {
	case "script", "style", "noscript", "template", "svg", "iframe", "canvas", "object":
		return // 不可见/无文本语义
	case "br":
		b.WriteString("\n")
		return
	case "h1", "h2", "h3", "h4", "h5", "h6":
		level := int(n.Data[1] - '0')
		b.WriteString("\n" + strings.Repeat("#", level) + " " + inlineText(n) + "\n\n")
		return
	case "pre":
		b.WriteString("\n```\n" + preText(n) + "\n```\n\n")
		return
	case "ol":
		// 有序列表：浏览器渲染的序号（1. 2. …）不在 DOM 文本中，渲染时补回。
		// 支持 start 属性；嵌套 ol 递归时独立计数。JS 端 pageconvert.js 同构实现。
		// li 走通用渲染（块级换行包裹），序号插到吃掉前置换行后的内容前，
		// 保证 "1. 内容" 同行。
		startNum := attrInt(n, "start", 1)
		b.WriteString("\n")
		i := startNum
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode && strings.EqualFold(c.Data, "li") {
				var lb strings.Builder
				renderNode(c, &lb, st)
				b.WriteString(strconv.Itoa(i) + ". " + strings.TrimLeft(lb.String(), "\n"))
				i++
				continue
			}
			renderNode(c, b, st)
		}
		b.WriteString("\n")
		return
	}
	if isBlockTag(n.Data) {
		b.WriteString("\n")
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			renderNode(c, b, st)
		}
		b.WriteString("\n")
		return
	}
	// 行内元素：递归，不换行
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		renderNode(c, b, st)
	}
}

// isBlockTag 块级元素：渲染时强制换行分隔（标题/pre 已单独处理）
func isBlockTag(tag string) bool {
	switch tag {
	case "p", "div", "li", "ul", "ol", "dl", "dt", "dd", "table", "thead", "tbody",
		"tr", "td", "th", "section", "article", "aside", "header", "footer", "main",
		"nav", "blockquote", "figure", "figcaption", "address", "form", "fieldset",
		"hr", "details", "summary", "button", "select", "option", "video", "audio":
		return true
	}
	return false
}

// inlineText 标题内文本（标题内不再嵌套块级结构，取纯文本即可）
func inlineText(n *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(c *html.Node) {
		if c.Type == html.TextNode {
			sb.WriteString(compactSpaces(c.Data))
		}
		for x := c.FirstChild; x != nil; x = x.NextSibling {
			walk(x)
		}
	}
	walk(n)
	return strings.TrimSpace(sb.String())
}

// preText 代码块文本：保留原始换行（代码语义），仅去掉首尾空白
func preText(n *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(c *html.Node) {
		if c.Type == html.TextNode {
			sb.WriteString(c.Data)
		}
		for x := c.FirstChild; x != nil; x = x.NextSibling {
			walk(x)
		}
	}
	walk(n)
	return strings.TrimSpace(sb.String())
}

func findFirst(n *html.Node, tag string) *html.Node {
	if n.Type == html.ElementNode && n.Data == tag {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if r := findFirst(c, tag); r != nil {
			return r
		}
	}
	return nil
}

func findTitle(doc *html.Node) string {
	head := findFirst(doc, "head")
	if head == nil {
		return ""
	}
	if t := findFirst(head, "title"); t != nil {
		return strings.TrimSpace(inlineText(t))
	}
	return ""
}

// compactSpaces 折叠空白：连续空白（含换行）压成单个空格
func compactSpaces(s string) string {
	var b strings.Builder
	inSpace := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' || r == '\v' {
			if !inSpace {
				b.WriteByte(' ')
				inSpace = true
			}
		} else {
			b.WriteRune(r)
			inSpace = false
		}
	}
	return b.String()
}

// collapseBlankLines 连续 3+ 换行压成 2 个（一个空行）
func collapseBlankLines(s string) string {
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return s
}
