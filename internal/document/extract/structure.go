// structure.go 结构进元数据（F3a，2026-11-18 评审契约）：docling 只贡献章节树
// （层级/标题/页号），正文一个字节不动——块边界/分词/块内容与 plain 基线逐位
// 一致。匹配逻辑与旧叠加版同款（页窗 ±2、字符多重集 ≥60%、密度门、匹配率门、
// 目录行排除、同行去重），产出从「注入 # 前缀」改为记录 HeadingMark 元数据
// （rune 偏移 + 层级 + 标题 + 面包屑链），随正文落库；BuildIndex 切块后按中点
// 规则把面包屑赋给 chunk 的 SectionPath（经 FTS 加权与向量化前置语境生效）。
//
// 两种匹配模式（按择优文本形态自动选择）：
//   - 分页文本（含 \f，pdftotext 产物）：标题只在所属页 ±2 页窗口内匹配，
//     页号来自 docling 章节树，精度高；
//   - 连续文本（无页界，主路 httpfetch 产物）：全文本短行扫描，目录行
//     （点引导线）排除，防标题匹配到目录页。
//
// 质量门（Phase 0 实测校准，与旧叠加版一致）：
//   - 标题密度 0.05~2/页：《人月神话》3 个（0.007）、
//     《Go 程序设计语言》5495 个（逐字空格误判）都会被门挡掉；
//   - 匹配率 ≥30% 才采纳，避免未命中的乱序标题产出伪结构；
//   - 标题含 U+FFFD 直接跳过（标题字体缺映射的书，如《人月神话》）。
//
// 匹配用字符多重集相似度（无序），专治 docling 行内乱序标题——Phase 0 实测
// AgenticSE 标题字符乱序但字符集不变（"1.1但绝非施工现场氛围编程自有其位，"）。
package extract

import (
	"regexp"
	"sort"
	"strings"
	"unicode"

	docling "github.com/unitedrhino/docling"
)

// 结构匹配质量门常量（与旧叠加版一致）。
const (
	structMinDensity   = 0.05 // 最小标题密度（个/页）：低于此视为无有效章节结构
	structMaxDensity   = 2.0  // 最大标题密度：高于此视为标题误判泛滥
	structMinMatch     = 0.30 // 最低匹配率：低于此放弃 marks
	structMinSim       = 0.60 // 单标题模糊匹配阈值（字符重合度）
	structPageWindow   = 2    // 页号匹配窗口（±页）
	structMinTitle     = 4    // 标题最短 rune 数（过短误判率高）
	structMaxLineRunes = 60   // 候选行长度上限（标题都是短行，长行跳过省算力）
)

// tocLineRe 目录行特征：点引导线（"......"）——全文本模式下排除，防止章节
// 标题匹配到目录页而非正文位置。
var tocLineRe = regexp.MustCompile(`\.{6,}|…{3,}`)

// HeadingMark docling 章节标题在正文中的定位标记（F3a 元数据）。不变式：
// text[mark.Offset:] 以 mark.Title 开头（生成与 TOC 剥离共同维护，剥离删除
// 标题所在行时连 mark 一起丢弃，其余偏移前移修正）。
type HeadingMark struct {
	Offset int      `json:"offset"` // 标题行首（去前导空白）rune 偏移
	Level  int      `json:"level"`  // 1..6（docling TextLevel 截断）
	Title  string   `json:"title"`  // 正文标题行文本（去首尾空白）
	Path   []string `json:"path"`   // 完整面包屑链（含自身，v4 breadcrumbPaths 同款层级栈；原始未截断，赋值时按 v4 规则截断）
}

// docSection docling 产出的章节标记（页号 0 起，与 pdftotext 的 \f 分页对齐）。
type docSection struct {
	page  int
	level int
	title string
}

// doclingSections 从 PDF 字节提取章节标记与总页数：docling 解析 → content_list
// 过滤标题项（TextLevel≥1，剔除页眉/页脚/目录标签与乱码标题）。解析失败返回
// nil, 0。
func doclingSections(data []byte) ([]docSection, int) {
	doc, err := docling.ParsePDFWithOptions(data, docling.PDFOptions{
		DisableEmbeddedImageExtraction: true, // 结构匹配不需要图片像素，省时省内存
	})
	if err != nil || doc == nil {
		return nil, 0
	}
	items := docling.ToContentList(doc, docling.SourceGolight)
	secs := make([]docSection, 0, len(items)/4)
	for _, it := range items {
		if it.TextLevel < 1 || it.PageIdx < 0 {
			continue
		}
		switch it.Label {
		case "page_header", "page_footer", "document_index":
			continue
		}
		title := strings.TrimSpace(strings.Join(it.SectionPath, " > "))
		if title == "" {
			title = strings.TrimSpace(it.Text)
		}
		title = strings.ReplaceAll(title, "\uFFFD", "")
		if title == "" {
			continue
		}
		secs = append(secs, docSection{page: int(it.PageIdx), level: int(it.TextLevel), title: title})
	}
	sort.SliceStable(secs, func(i, j int) bool { return secs[i].page < secs[j].page })
	return secs, len(doc.Pages)
}

// MarksForDiag 诊断/评测入口：对指定文本应用 docling 章节匹配，返回标题
// marks 与命中数（不修改正文）。生产路径在 extract.go（v7 起所有拿到 PDF
// 字节的文档都生成 marks），不走此函数；本入口仅供实验手工构造 marks 用。
func MarksForDiag(data []byte, text string) ([]HeadingMark, int) {
	secs, docPages := doclingSections(data)
	return overlayMarks(text, secs, docPages)
}

// markCand 候选标题行：文本 + 行首（去前导空白）rune 偏移 + 预计算字符计数。
type markCand struct {
	page   int // 分页模式页号；连续文本 0
	offset int // 行首（去前导空白）rune 偏移
	text   string
	counts map[rune]int
}

// markHit 命中：候选行 + docling 层级。
type markHit struct {
	offset, level int
	title         string
}

// overlayMarks 把 docling 章节树匹配到正文行，返回 HeadingMark 元数据与命中
// 数。正文不做任何修改。返回 (nil, 0) 表示放弃（质量门未过 / 页号不可信）。
func overlayMarks(text string, secs []docSection, docPages int) ([]HeadingMark, int) {
	if len(secs) == 0 || docPages <= 0 {
		return nil, 0
	}
	density := float64(len(secs)) / float64(docPages)
	if density < structMinDensity || density > structMaxDensity {
		return nil, 0
	}

	// 候选行收集（一次扫描）：分页文本按页组织（页窗匹配用，\f 计 1 rune），
	// 连续文本平铺（全文匹配用，目录行排除）
	paged := strings.Contains(text, "\f")
	var cands []markCand
	if paged {
		pages := strings.Split(text, "\f")
		if len(pages) < docPages/2 { // 页数严重对不上：页号映射不可信
			return nil, 0
		}
		base := 0 // 当前页首行 rune 偏移（页间 \f 计 1）
		for pi, page := range pages {
			off := base
			for _, line := range strings.Split(page, "\n") {
				t, toff := trimLeftRunes(line, off)
				if candidateLine(t) {
					cands = append(cands, markCand{page: pi, offset: toff, text: t, counts: charCounts(t)})
				}
				off += len([]rune(line)) + 1 // +1 换行
			}
			base = off // 下一页起点：本页全部行 +1（覆盖页间 \f）
		}
	} else {
		off := 0
		for _, line := range strings.Split(text, "\n") {
			t, toff := trimLeftRunes(line, off)
			if candidateLine(t) && !tocLineRe.MatchString(t) {
				cands = append(cands, markCand{offset: toff, text: t, counts: charCounts(t)})
			}
			off += len([]rune(line)) + 1
		}
	}

	type usedKey = int // 命中行 offset 唯一，去重直接按偏移
	used := map[usedKey]bool{}
	hits := make([]markHit, 0, len(secs))
	for _, s := range secs {
		if s.title == "" || len([]rune(s.title)) < structMinTitle || strings.ContainsRune(s.title, 0xFFFD) {
			continue
		}
		level := s.level
		if level < 1 {
			level = 1
		}
		if level > 6 {
			level = 6
		}
		titles := charCounts(s.title)
		bestOff, bestScore := -1, 0.0
		var bestCand markCand
		for _, c := range cands {
			if paged && (c.page < s.page-structPageWindow || c.page > s.page+structPageWindow) {
				continue // 分页模式：只在所属页 ±2 页窗口内匹配
			}
			if used[c.offset] {
				continue
			}
			if sim := multisetSim(titles, c.counts); sim > bestScore {
				bestOff, bestScore, bestCand = c.offset, sim, c
			}
		}
		if bestOff < 0 || bestScore < structMinSim {
			continue
		}
		used[bestOff] = true
		hits = append(hits, markHit{offset: bestOff, level: level, title: bestCand.text})
	}
	if len(hits) == 0 || float64(len(hits))/float64(len(secs)) < structMinMatch {
		return nil, 0
	}

	// 按文档序（偏移序）用 v4 同款层级栈构建面包屑链
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].offset < hits[j].offset })
	type node struct {
		level int
		title string
	}
	var stack []node
	marks := make([]HeadingMark, 0, len(hits))
	for _, h := range hits {
		for len(stack) > 0 && stack[len(stack)-1].level >= h.level {
			stack = stack[:len(stack)-1]
		}
		stack = append(stack, node{h.level, h.title})
		path := make([]string, 0, len(stack))
		for _, nd := range stack {
			path = append(path, nd.title)
		}
		marks = append(marks, HeadingMark{Offset: h.offset, Level: h.level, Title: h.title, Path: path})
	}
	return marks, len(marks)
}

// trimLeftRunes 去行首空白：返回 TrimSpace 后文本与首非空白 rune 的偏移
// （base + 前导空白 rune 数）——保证 text[offset:] 以返回文本开头。
func trimLeftRunes(line string, base int) (string, int) {
	lead := 0
	for _, r := range line {
		if !unicode.IsSpace(r) {
			break
		}
		lead++
	}
	return strings.TrimSpace(line), base + lead
}

// candidateLine 候选标题行：非空、长度合理、非已有 markdown 标题、非目录行。
func candidateLine(t string) bool {
	if len([]rune(t)) < structMinTitle || len([]rune(t)) > structMaxLineRunes {
		return false
	}
	if strings.ContainsRune(t, '#') || tocLineRe.MatchString(t) {
		return false
	}
	return true
}

// multisetSim 预计算计数版的字符多重集相似度。
func multisetSim(a, b map[rune]int) float64 {
	inter, la, lb := 0, 0, 0
	for r, n := range a {
		la += n
		if m, ok := b[r]; ok {
			inter += min(n, m)
		}
	}
	for _, n := range b {
		lb += n
	}
	if la+lb == 0 {
		return 0
	}
	return 2 * float64(inter) / float64(la+lb)
}

// charMultisetSim 字符多重集相似度（无序）：2×交集 / (|a|+|b|)，忽略空格与
// U+FFFD。专治 docling 行内乱序标题——字符集不变、顺序打乱。
func charMultisetSim(a, b string) float64 {
	return multisetSim(charCounts(a), charCounts(b))
}

// charCounts rune 多重集（忽略空格与 U+FFFD）。
func charCounts(s string) map[rune]int {
	m := map[rune]int{}
	for _, r := range s {
		if r == ' ' || r == '\t' || r == 0xFFFD {
			continue
		}
		m[r]++
	}
	return m
}
