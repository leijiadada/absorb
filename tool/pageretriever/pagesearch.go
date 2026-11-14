package retriever

import (
	"fmt"
	"sort"
	"strings"
)

// 本文件在缓存的结构化页面文本上做定向检索，不产生浏览器往返。
// compact 匹配忽略大小写与空白：「多 Agent Runner」可命中「多Agent runner」。

const (
	searchHitSectionMax = 3500  // 标题命中时返回的 section 上限
	searchWindowAround  = 1500  // 正文命中时的窗口半径
	refMatWindowRadius  = 2000  // 参考材料邻近窗口半径
	fullPageMax         = 30000 // 全文上限
)

// compactIndex 在 s 中查找 sub 的首个出现位置（大小写/空白不敏感），
// 返回原串中的字节下标，未找到返回 -1。
// 先把 s 压成「去空白 + 小写」的紧凑串并记下每字符的原串下标，再在紧凑串上做子串查找。
func compactIndex(s, sub string) int {
	var comp, idx []int32 // comp: 紧凑串 rune；idx: 对应原串字节下标
	lower := strings.ToLower(s)
	for i, r := range lower {
		// 与 compactStr 同语义：空白与管道符（表格结构符号）不参与匹配
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' || r == '\v' || r == '|' {
			continue
		}
		comp = append(comp, r)
		idx = append(idx, int32(i))
	}
	needle := []rune(compactStr(strings.ToLower(sub)))
	if len(needle) == 0 || len(needle) > len(comp) {
		return -1
	}
	for i := 0; i+len(needle) <= len(comp); i++ {
		match := true
		for j := range needle {
			if comp[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return int(idx[i])
		}
	}
	return -1
}

func compactStr(s string) string {
	var b strings.Builder
	for _, r := range s {
		// | 为管道表格结构符号（见 pageextract.renderPipeTable），非内容字符
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' || r == '\v' || r == '|' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func compactContains(s, sub string) bool {
	return compactIndex(s, sub) >= 0
}

// headingLine 结构化文本中的标题行
type headingLine struct {
	text  string // 原始行（含 # 前缀）
	level int    // 1-6
	start int    // 行起始字节下标（全文中）
}

// Text 返回标题原始行文本（含 # 前缀），供外部包读取 ParseHeadings 结果。
func (h headingLine) Text() string { return h.text }

// ParseHeadings 解析全文标题行（^#{1,6} 开头）。
// 围栏状态机与 bodySegments 同语义（TrimSpace 后 == "```" 翻转）：代码块内的
// # 行（shell/python/yaml 注释）是内容不是标题，误判会污染 headingLine 列表，
// 导致 sectionFrom 以伪标题为起点切碎真实章节。
func ParseHeadings(pageText string) []headingLine {
	var out []headingLine
	start := 0
	inCode := false
	for start <= len(pageText) {
		end := strings.IndexByte(pageText[start:], '\n')
		var line string
		next := len(pageText)
		if end >= 0 {
			line = pageText[start : start+end]
			next = start + end + 1
		} else {
			line = pageText[start:]
		}
		switch {
		case strings.HasPrefix(strings.TrimSpace(line), "```"):
			// 围栏开/关行（裸 ``` 或 ```lang 均算）。转换器产出现为裸 ```，
			// HasPrefix 兼容未来带语言标注的来源；不误伤普通正文行。
			inCode = !inCode
		case !inCode:
			if lvl, ok := headingLevel(line); ok {
				out = append(out, headingLine{text: line, level: lvl, start: start})
			}
		}
		if end < 0 {
			break
		}
		start = next
	}
	return out
}

func headingLevel(line string) (int, bool) {
	trimmed := strings.TrimLeft(line, " ")
	n := 0
	for n < len(trimmed) && trimmed[n] == '#' {
		n++
	}
	if n >= 1 && n <= 6 && n < len(trimmed) && trimmed[n] == ' ' {
		return n, true
	}
	return 0, false
}

// SearchPageText 收集所有与关键词匹配的页面内容，合并后返回。
// refMat 非空时用于去重：命中内容与参考材料完全一致时跳过。
func SearchPageText(pageText string, keywords []string, refMat string) (string, bool) {
	kws := TrimStrings(keywords)
	if pageText == "" || len(kws) == 0 {
		return "", false
	}
	refCompact := compactStr(refMat)

	var results []string

	// 标题行命中。
	headings := ParseHeadings(pageText)
	var matched []headingLine
	for _, h := range headings {
		text := headingText(h.text)
		for _, k := range kws {
			if compactContains(text, k) {
				matched = append(matched, h)
				break
			}
		}
	}
	// 按级别降序：父节包含子节时，父节会被后续去重跳过。
	sort.Slice(matched, func(i, j int) bool { return matched[i].level > matched[j].level })
	for _, h := range matched {
		section := sectionFrom(pageText, h)
		if section == "" {
			continue
		}
		if refCompact != "" && compactStr(section) == refCompact {
			continue
		}
		if isContainedIn(results, section) {
			continue
		}
		section = truncateAround(section, kws, searchHitSectionMax)
		results = append(results, fmt.Sprintf("【页面检索：关键词命中标题「%s」】\n%s", headingText(h.text), section))
	}

	// 正文段落命中。
	for _, seg := range bodySegments(pageText) {
		matchedKW := ""
		for _, k := range kws {
			if compactContains(seg.text, k) {
				matchedKW = k
				break
			}
		}
		if matchedKW == "" {
			continue
		}
		// 与参考材料重叠的行跳过（返回已知内容无意义）
		if refCompact != "" && (compactContains(seg.text, refMat) || compactContains(refMat, seg.text)) {
			continue
		}
		win := windowAround(pageText, seg.start, seg.start+len(seg.text))
		if isContainedIn(results, win) {
			continue
		}
		results = append(results, fmt.Sprintf("【页面检索：关键词「%s」命中段落】\n%s", matchedKW, win))
	}

	// 全文 compact 兜底。
	for _, k := range kws {
		if i := compactIndex(pageText, k); i >= 0 {
			win := windowAround(pageText, i, i+len(k))
			if isContainedIn(results, win) {
				continue
			}
			results = append(results, fmt.Sprintf("【页面检索：关键词「%s」命中页面文本】\n%s", k, win))
		}
	}

	if len(results) == 0 {
		return "", false
	}
	return strings.Join(results, "\n\n---\n\n"), true
}

// headingText 提取标题行的纯文本（去掉 # 前缀）
func headingText(line string) string {
	return strings.TrimSpace(line[strings.Index(line, "#")+countHash(line):])
}

// isContainedIn 检查 content 是否已被 results 中的某个条目覆盖（双向 compact 比较）。
// 规则：新内容被已有结果包含（已有更精确），或已有结果被新内容包含（新内容更宽泛），均视为重复。
func isContainedIn(results []string, content string) bool {
	c := compactStr(content)
	if c == "" {
		return true
	}
	for _, r := range results {
		idx := strings.Index(r, "\n")
		if idx < 0 {
			continue
		}
		existingCompact := compactStr(r[idx+1:])
		if strings.Contains(existingCompact, c) || strings.Contains(c, existingCompact) {
			return true
		}
	}
	return false
}

// sectionFrom 取标题到下一个同级/更高级标题（或代码围栏边界外的标题行）之间的内容
func sectionFrom(pageText string, h headingLine) string {
	headings := ParseHeadings(pageText)
	end := len(pageText)
	for _, x := range headings {
		if x.start > h.start && x.level <= h.level {
			end = x.start
			break
		}
	}
	return strings.TrimSpace(pageText[h.start:end])
}

// bodySegments 正文段：非标题、非代码围栏、非空行（以空行分隔的文本块）
type bodySeg struct {
	text  string
	start int
}

func bodySegments(pageText string) []bodySeg {
	var out []bodySeg
	lines := strings.Split(pageText, "\n")
	inCode := false
	pos := 0
	var cur strings.Builder
	curStart := -1
	flush := func(end int) {
		if t := strings.TrimSpace(cur.String()); t != "" && curStart >= 0 {
			out = append(out, bodySeg{text: t, start: curStart})
		}
		cur.Reset()
		curStart = -1
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			flush(pos)
			inCode = !inCode
			pos += len(line) + 1
			continue
		}
		if inCode {
			pos += len(line) + 1
			continue
		}
		if trimmed == "" {
			flush(pos)
			pos += len(line) + 1
			continue
		}
		if _, isHead := headingLevel(trimmed); isHead {
			flush(pos)
			pos += len(line) + 1
			continue
		}
		if curStart < 0 {
			curStart = pos
		}
		if cur.Len() > 0 {
			cur.WriteString(" ")
		}
		cur.WriteString(trimmed)
		pos += len(line) + 1
	}
	flush(pos)
	return out
}

// windowAround 全文中 [start,end) 附近 ±around 的窗口（带省略号）。
// 窗口左右边界在 ±around 范围内吸附到最近的标题行：若最近标题在范围内，
// 左边界取到标题起始、右边界取到标题之前，避免窗口跨越标题把无关章节/导航带进来。
func windowAround(pageText string, start, end int) string {
	s, e := snapWindowToHeadings(pageText, start, end, searchWindowAround)
	var b strings.Builder
	if s > 0 {
		b.WriteString("…")
	}
	b.WriteString(pageText[s:e])
	if e < len(pageText) {
		b.WriteString("…")
	}
	return b.String()
}

// snapWindowToHeadings 把 [start,end) 的窗口边界吸附到邻近标题行，避免窗口跨标题带入无关章节。
// 仅当标题行落在 ±around 范围内才吸附，远离时不强行扩窗口。
func snapWindowToHeadings(pageText string, start, end, around int) (int, int) {
	s := max(0, start-around)
	e := min(len(pageText), end+around)
	headings := ParseHeadings(pageText)

	// 左边界：start 之前最近的标题（start <= pos）
	var left *headingLine
	for i := range headings {
		if headings[i].start <= start {
			left = &headings[i]
		} else {
			break
		}
	}
	// 右边界：end 之后最近的标题（pos >= end），取第一个
	var right *headingLine
	for i := range headings {
		if headings[i].start >= end {
			right = &headings[i]
			break
		}
	}

	if left != nil && left.start >= s && left.start < start {
		s = left.start
	}
	if right != nil && right.start <= e && right.start > end {
		e = right.start
	}
	return s, e
}

// truncateAround 超长内容截断：取关键词出现处 ±radius 窗口。
// 优先在正文（跳过首行标题）定位关键词——标题命中只说明节相关，正文出现处才是论述主体；
// 正文无关键词（只出现在标题）时回退到全文定位。
func truncateAround(s string, keywords []string, limit int) string {
	if len(s) <= limit {
		return s
	}
	body, skip := s, 0
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		body, skip = s[i:], i
	}
	for _, k := range keywords {
		if i := compactIndex(body, k); i >= 0 {
			return sliceWindow(s, skip+i)
		}
	}
	for _, k := range keywords {
		if i := compactIndex(s, k); i >= 0 {
			return sliceWindow(s, i)
		}
	}
	return s[:limit] + "…"
}

// sliceWindow s 中 pos 附近 ±refMatWindowRadius 的窗口（带省略号）
func sliceWindow(s string, pos int) string {
	sr := max(0, pos-refMatWindowRadius)
	er := min(len(s), pos+refMatWindowRadius)
	var b strings.Builder
	if sr > 0 {
		b.WriteString("…")
	}
	b.WriteString(s[sr:er])
	if er < len(s) {
		b.WriteString("…")
	}
	return b.String()
}

// RefMatWindow 在全文中定位参考材料并返回 ±radius 窗口；定位失败退化为全文前 4000 字符。
func RefMatWindow(pageText, refMat string) string {
	if pageText == "" {
		return ""
	}
	if i := compactIndex(pageText, refMat); i >= 0 {
		end := min(len(pageText), i+len(refMat)+refMatWindowRadius)
		s := max(0, i-refMatWindowRadius)
		var b strings.Builder
		if s > 0 {
			b.WriteString("…")
		}
		b.WriteString(pageText[s:end])
		if end < len(pageText) {
			b.WriteString("…")
		}
		return b.String()
	}
	// 参考材料定位失败（如选自 iframe/渲染改写）：取全文前 4000
	if len(pageText) > 4000 {
		return pageText[:4000] + "…"
	}
	return pageText
}

// ZeroHitSignal 零命中信号：页面（缓存全文）未出现关键词
func ZeroHitSignal(keywords []string) string {
	return "【页面检索：关键词 " + quoteJoin(TrimStrings(keywords)) +
		" 在本页未出现。页面没有展开该概念，请基于自身知识回答，不要再调用本工具。】"
}

// TrimStrings 去除空白后非空的字符串列表
func TrimStrings(in []string) []string {
	var out []string
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func quoteJoin(ss []string) string {
	parts := make([]string, len(ss))
	for i, s := range ss {
		parts[i] = "\"" + s + "\""
	}
	return strings.Join(parts, "、")
}

func countHash(line string) int {
	n := 0
	for n < len(line) && line[n] == '#' {
		n++
	}
	return n
}
