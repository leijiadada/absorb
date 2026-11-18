package extract

// 目录页剥离（P5，2026-11-18）+ marks 一致性维护（F3a）：纯规则两级剥离，
// 零模型调用。
//
// 问题（实测证据见 design/5.md）：书的目录页被当成正文切块入库——目录行
// 「模式：合并就绪包 . . . . . . . . . 141」在检索中产生噪音命中挤占 top-10、
// 目录块继承前言标题的错位面包屑、每本书数百个 junk 块白耗嵌入 API 和向量
// 空间。行级剥离对主路书还有联动收益：docling 标题匹配不再有目录页候选。
//
// F3a（2026-11-18）：marks 感知——结构叠加已改为元数据（HeadingMark.Offset
// 指向正文 rune 偏移），本层删除页/行时必须同步维护：落在被删页/行上的
// marks 丢弃（标题行没了，面包屑失去锚点），其余 marks 偏移减去删除前缀的
// rune 数。不变式：剥离后每个 mark.Offset 处的原文以 mark.Title 开头。
// marks 为 nil（HTML 路径 / 质量门未过）时行为与纯文本剥离完全一致。
//
// 管线位置：结构匹配之后——页级删除会移除分页符使后续页号前移，若在匹配
// 之前执行会破坏 docling 标题 ±2 页匹配窗口；匹配自身已有 tocLineRe
// 排除目录行，此处对最终文本再剥一次。
//
// 两级策略：
//  1. 页级（仅 poppler 分页文本）：整页非空行中目录条目占比 ≥30% 且 ≥10 行
//     → 整页删除（清掉行级残留的章组标题「第 3 章」、页眉「目 录」碎渣）。
//  2. 行级（全部路径）：删除点引导线/省略号引导 + 行尾页码的目录条目行。
//
// 护栏：整书删除页数超过总页数 15% 时判定为误伤，放弃页级只保留行级；
// 行级跳过围栏代码块内部（提取期误删不可恢复，代码行是最大的误伤来源）。

import (
	"regexp"
	"sort"
	"strings"
)

// tocEntryRe 目录条目行：标题文本 + 引导线 + 行尾页码（1~4 位）。
// 三种引导形态：空格分隔点串（". . . . "，poppler 渲染常见）、连续点串
// （"......"）、中文省略号（"……"）。页码必须在行尾（允许尾随空白）——
// 正文里 "版本 1.2.3.4"（点后跟数字而非行尾）与 "等待……"（无行尾页码）
// 均不匹配；代码行 fmt.Println("a....1") 因行尾是引号不匹配。
var tocEntryRe = regexp.MustCompile(`(?:(?:\.\s*){3,}|\.{4,}|(?:…\s*){2,})\d{1,4}\s*$`)

// tocFenceRe 围栏标记行（``` 或 ~~~ 开头）——行级剥离跳过围栏内部。
var tocFenceRe = regexp.MustCompile("^(```|~~~)")

// 页级判定与护栏参数。
const (
	tocPageDensity   = 0.30 // 页内非空行中目录条目行占比阈值
	tocPageMinLines  = 10   // 参与判定的最少非空行数（短页不判，防误伤版权页等）
	tocMaxDroppedPct = 0.15 // 整书删除页数占总页数上限，超出视为误伤
)

// stripTOC 目录剥离主入口（F3a marks 感知）：页级（有分页符时）+ 行级，
// 返回剥离后文本与偏移修正后的 marks。
func stripTOC(content string, marks []HeadingMark) (string, []HeadingMark) {
	if strings.Contains(content, "\f") {
		content, marks, _ = dropTOCPagesMarks(content, marks)
	}
	content, marks, _ = stripTOCLinesMarks(content, marks)
	return content, marks
}

// stripTOCDiag 带统计版（测试与诊断用）：返回剥离后文本、删除页数、删除行数。
func stripTOCDiag(content string) (string, int, int) {
	pagesDropped := 0
	if strings.Contains(content, "\f") {
		content, _, pagesDropped = dropTOCPagesMarks(content, nil)
	}
	linesDropped := 0
	content, _, linesDropped = stripTOCLinesMarks(content, nil)
	return content, pagesDropped, linesDropped
}

// dropTOCPagesMarks 页级删除：按 \f 分页，先统计全部密度达标页——数量超过
// 总页数 15% 上限时判定为误伤（正常书籍的目录页远低于此占比），放弃页级
// 整体回退（只做行级）；未超限则全部删除。落在我们删除页上的 marks 一并
// 丢弃，其余按页前缀 rune 数（含页间 \f）前移修正。
func dropTOCPagesMarks(content string, marks []HeadingMark) (string, []HeadingMark, int) {
	pages := strings.Split(content, "\f")
	lens := make([]int, len(pages))
	starts := make([]int, len(pages)) // 页首 rune 偏移（页间 \f 计 1）
	at := 0
	for i, pg := range pages {
		starts[i] = at
		lens[i] = len([]rune(pg))
		at += lens[i] + 1
	}
	var tocIdx []int
	for i, pg := range pages {
		if isTOCPage(pg) {
			tocIdx = append(tocIdx, i)
		}
	}
	if len(tocIdx) == 0 || float64(len(tocIdx)) > float64(len(pages))*tocMaxDroppedPct {
		return content, marks, 0
	}
	drop := make(map[int]bool, len(tocIdx))
	for _, i := range tocIdx {
		drop[i] = true
	}
	// shift[i] = 页 i 之前累计删除的 rune 数（被删页各贡献 len+1，含 \f）
	shift := make([]int, len(pages))
	acc := 0
	for i := range pages {
		shift[i] = acc
		if drop[i] {
			acc += lens[i] + 1
		}
	}
	out := make([]HeadingMark, 0, len(marks))
	for _, m := range marks {
		pi := sort.Search(len(starts), func(j int) bool { return starts[j] > m.Offset }) - 1
		if pi < 0 {
			pi = 0
		}
		if drop[pi] {
			continue // 标题行随页删除
		}
		m.Offset -= shift[pi]
		out = append(out, m)
	}
	return strings.Join(keepPages(pages, drop), "\f"), out, len(tocIdx)
}

// keepPages 保留未删除页（与旧实现逐字节一致：保留页以 \f 连接）。
func keepPages(pages []string, drop map[int]bool) []string {
	kept := make([]string, 0, len(pages))
	for i, pg := range pages {
		if !drop[i] {
			kept = append(kept, pg)
		}
	}
	return kept
}

// isTOCPage 页级判定：非空行 ≥ tocPageMinLines 且目录条目行占比 ≥ tocPageDensity。
func isTOCPage(pg string) bool {
	nonEmpty, tocLines := 0, 0
	for _, ln := range strings.Split(pg, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		nonEmpty++
		if tocEntryRe.MatchString(t) {
			tocLines++
		}
	}
	if nonEmpty < tocPageMinLines {
		return false
	}
	return float64(tocLines)/float64(nonEmpty) >= tocPageDensity
}

// stripTOCLines 行级剥离（纯文本，测试入口）：删除围栏外的目录条目行。
func stripTOCLines(content string) string {
	out, _, _ := stripTOCLinesMarks(content, nil)
	return out
}

// stripTOCLinesMarks 行级剥离（marks 感知）：删除围栏外的目录条目行，落在
// 被删行上的 marks 丢弃，其余按行前缀 rune 数（含换行）前移修正。围栏状态
// 逐行翻转（``` / ~~~），围栏内的行原样保留。
func stripTOCLinesMarks(content string, marks []HeadingMark) (string, []HeadingMark, int) {
	lines := strings.Split(content, "\n")
	offs := make([]int, len(lines)) // 行首 rune 偏移（换行计 1）
	at := 0
	for i, ln := range lines {
		offs[i] = at
		at += len([]rune(ln)) + 1
	}
	inFence := false
	dropped := 0
	drop := make([]bool, len(lines))
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if tocFenceRe.MatchString(t) {
			inFence = !inFence
		}
		if !inFence && tocEntryRe.MatchString(t) {
			drop[i] = true
			dropped++
		}
	}
	if dropped == 0 {
		return content, marks, 0
	}
	// shift[i] = 行 i 之前累计删除的 rune 数（被删行各贡献 len+1，含换行）
	shift := make([]int, len(lines))
	acc := 0
	for i := range lines {
		shift[i] = acc
		if drop[i] {
			acc += len([]rune(lines[i])) + 1
		}
	}
	out := make([]HeadingMark, 0, len(marks))
	for _, m := range marks {
		li := sort.Search(len(offs), func(j int) bool { return offs[j] > m.Offset }) - 1
		if li < 0 {
			li = 0
		}
		if drop[li] {
			continue // 标题行被剥离（目录行不该有 mark，防御性兜底）
		}
		m.Offset -= shift[li]
		out = append(out, m)
	}
	var b strings.Builder
	for i, ln := range lines {
		if drop[i] {
			continue
		}
		b.WriteString(ln)
		b.WriteByte('\n')
	}
	result := b.String()
	if !strings.HasSuffix(content, "\n") { // 保持原文末尾换行状态
		result = strings.TrimSuffix(result, "\n")
	}
	return result, out, dropped
}
