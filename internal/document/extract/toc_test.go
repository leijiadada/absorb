package extract

// TOC 剥离（P5）：页级密度判定、行级条目剥离、围栏保护、护栏与误伤防线。
import (
	"strings"
	"testing"
)

// tocLine 构造一条目录条目行（空格点引导 + 行尾页码）。
func tocLine(title string, page string) string {
	return title + " . . . . . . . . " + page
}

// TestStripTOCPageLevelDropsTOCPage 页级删除：密度达标的目录页整页移除，
// 正文页逐字保留。
func TestStripTOCPageLevelDropsTOCPage(t *testing.T) {
	var toc strings.Builder
	toc.WriteString("目 录\n")
	for i := 1; i <= 15; i++ {
		toc.WriteString(tocLine("第 "+itoa(i)+" 节 标题内容", itoa(i+10)))
		toc.WriteString("\n")
	}
	body1 := strings.Repeat("第一章正文内容，讲的是上下文工程的核心思想。\n", 8)
	body2 := strings.Repeat("第二章正文内容， discussing agent pipelines。\n", 8)
	filler := strings.Repeat("填充正文页，避免目录页占比触发护栏。\n", 30)
	in := body1 + "\f" + strings.Repeat(filler+"\f", 9) + toc.String() + "\f" + body2

	out, pages, lines := stripTOCDiag(in)
	// 页级整删后条目行随页面消失，行级不再有可删行 → 1 页 / 0 行
	if pages != 1 || lines != 0 {
		t.Fatalf("diag: pages=%d lines=%d, want 1/0", pages, lines)
	}
	if strings.Contains(out, "标题内容") {
		t.Fatalf("TOC page not dropped:\n%s", out)
	}
	if !strings.Contains(out, body1[:40]) || !strings.Contains(out, body2[:40]) {
		t.Fatalf("body pages damaged")
	}
	if got := strings.Count(out, "\f"); got != 10 {
		t.Fatalf("form-feed count = %d, want 10（目录页连同分页符移除）", got)
	}
}

// TestStripTOCLineLevelOnly 无分页符文本（主路）：只做行级剥离。
func TestStripTOCLineLevelOnly(t *testing.T) {
	in := "正文开头。\n" + tocLine("1 智能体软件工程", "3") + "\n" +
		"目录条目省略号变体……141\n" + "版本 1.2.3.4 已发布。\n正文结尾。"
	out, pages, lines := stripTOCDiag(in)
	if pages != 0 || lines != 2 {
		t.Fatalf("diag: pages=%d lines=%d, want 0/2", pages, lines)
	}
	if strings.Contains(out, "智能体软件工程") || strings.Contains(out, "……141") {
		t.Fatalf("TOC lines not stripped:\n%s", out)
	}
	for _, keep := range []string{"正文开头。", "版本 1.2.3.4 已发布。", "正文结尾。"} {
		if !strings.Contains(out, keep) {
			t.Fatalf("body line lost: %q\n%s", keep, out)
		}
	}
}

// TestStripTOCFenceProtected 围栏代码块内的伪目录形态行不受剥离影响。
func TestStripTOCFenceProtected(t *testing.T) {
	in := "前文。\n```go\nfor i := 0; i < 3; i++ {\n    fmt.Println(\"step . . . 42\")\n}\n```\n" +
		tocLine("模式：合并就绪包", "141") + "\n后文。"
	out, _, lines := stripTOCDiag(in)
	if lines != 1 {
		t.Fatalf("stripped lines = %d, want 1（仅目录行）", lines)
	}
	if !strings.Contains(out, "fmt.Println(\"step . . . 42\")") {
		t.Fatalf("code line inside fence was stripped:\n%s", out)
	}
	if strings.Contains(out, "合并就绪包") {
		t.Fatalf("TOC line outside fence survived:\n%s", out)
	}
}

// TestStripTOCFalsePositives 误伤防线：常见正文形态不被行级剥离。
func TestStripTOCFalsePositives(t *testing.T) {
	cases := []string{
		"见第 1.2.3.4 节的详细说明。",    // 版本号式点串（点后是数字非行尾页码）
		"等待三秒……然后继续执行。",         // 省略号无行尾页码
		"重试 3 次。",               // 行尾数字但无引导线
		"如 3.4.1 所述……",          // 行尾省略号无页码
		`fmt.Println("a....1")`, // 行尾是引号
		"本章小结 . . . 详见附录",       // 点串后无页码
	}
	for _, c := range cases {
		out := stripTOCLines(c)
		if !strings.Contains(out, c) {
			t.Fatalf("false positive stripped: %q -> %q", c, out)
		}
	}
}

// TestStripTOCCapGuard 护栏：删除页数超 15% 上限时放弃页级（仅行级）。
func TestStripTOCCapGuard(t *testing.T) {
	var pages []string
	pages = append(pages, strings.Repeat("正文页零一二三四五六七八九十。\n", 12)) // p1
	for p := 0; p < 3; p++ {                                       // 3/7 ≈ 43% > 15% 上限
		var toc strings.Builder
		for i := 1; i <= 12; i++ {
			toc.WriteString(tocLine(itoa(p)+itoa(i)+" 条目标题", itoa(i)))
			toc.WriteString("\n")
		}
		pages = append(pages, toc.String())
	}
	pages = append(pages, strings.Repeat("正文页甲乙丙丁戊己庚辛壬癸子丑。\n", 12)) // p5
	pages = append(pages, strings.Repeat("正文页卯辰巳午未申酉戌亥。世界。\n", 12)) // p6
	pages = append(pages, strings.Repeat("正文页春夏秋冬东南西北中。\n", 12))    // p7
	in := strings.Join(pages, "\f")
	out, pg, _ := stripTOCDiag(in)
	if pg != 0 {
		t.Fatalf("cap guard failed: dropped %d pages, want 0 (超 15%% 上限回退)", pg)
	}
	// 行级仍生效
	if strings.Contains(out, "条目标题") {
		t.Fatalf("line-level strip should still work after page fallback")
	}
	if !strings.Contains(out, "正文页零") || !strings.Contains(out, "正文页春") {
		t.Fatalf("body damaged")
	}
}

// TestStripTOCTrailingNewlinePreserved 末尾无换行的文本剥离后保持原状态。
func TestStripTOCTrailingNewlinePreserved(t *testing.T) {
	withNl := "正文。\n" + tocLine("标题", "12") + "\n"
	if got := stripTOCLines(withNl); !strings.HasSuffix(got, "\n") {
		t.Fatalf("trailing newline lost: %q", got)
	}
	noNl := "正文。\n" + tocLine("标题", "12")
	if got := stripTOCLines(noNl); strings.HasSuffix(got, "\n") || !strings.HasSuffix(got, "正文。") {
		t.Fatalf("trailing state changed: %q", got)
	}
}

func itoa(n int) string {
	return string(rune('0' + n%10))
}

// ===== F3a：stripTOC 的 marks 一致性 =====

// TestStripTOCMarksPageDropped marks 落在目录页上 → 随页丢弃；目录页之后的
// marks 偏移前移修正（删除前缀 rune 数，含页间 \f）。不变式：剥离后每个
// mark.Offset 处的原文以 Title 开头。
func TestStripTOCMarksPageDropped(t *testing.T) {
	var toc strings.Builder
	toc.WriteString("目 录\n")
	for i := 1; i <= 15; i++ {
		toc.WriteString(tocLine("第 "+itoa(i)+" 节 标题内容", itoa(i+10)))
		toc.WriteString("\n")
	}
	body1 := strings.Repeat("第一章正文内容，讲的是上下文工程的核心思想。\n", 8)
	body2 := strings.Repeat("第二章正文内容， discussing agent pipelines。\n", 8)
	filler := strings.Repeat("填充正文页，避免目录页占比触发护栏。\n", 30)
	in := body1 + "\f" + strings.Repeat(filler+"\f", 9) + toc.String() + "\f" + body2

	// mark1 落在目录页（将随页丢弃）；mark2 落在目录页之后（偏移前移）
	markOnTOC := HeadingMark{Offset: runeLen(in) - runeLen(body2) - 1, Level: 1, Title: "第 5 节 标题内容"}
	markOnBody2 := HeadingMark{Offset: runeLen(in) - runeLen(body2), Level: 1, Title: "第二章正文内容"}
	_, marks := stripTOC(in, []HeadingMark{markOnTOC, markOnBody2})
	if len(marks) != 1 {
		t.Fatalf("marks = %d 个, want 1（目录页上的应丢弃）", len(marks))
	}
	m := marks[0]
	if m.Title != markOnBody2.Title {
		t.Fatalf("保留的 mark 错误：%q", m.Title)
	}
	outD, _, _ := stripTOCDiag(in)
	ors := []rune(outD)
	if !strings.HasPrefix(string(ors[m.Offset:]), m.Title) {
		t.Fatalf("偏移修正失败：out[%d:] = %q 不以 %q 开头", m.Offset, truncRunesForTOC(string(ors[m.Offset:]), 20), m.Title)
	}
}

// TestStripTOCMarksLineDropped 行级剥离：mark 落在目录条目行上 → 随行丢弃；
// 其后 marks 偏移前移修正。无分页符文本（主路产物同形态）。
func TestStripTOCMarksLineDropped(t *testing.T) {
	in := "正文开头。\n" + tocLine("1 智能体软件工程", "3") + "\n" +
		"2.1 章节标题甲乙\n正文段落。\n" + tocLine("2 另一条目内容", "9") + "\n结尾。"
	marks := []HeadingMark{
		{Offset: runeLen("正文开头。\n" + tocLine("1 智能体软件工程", "3") + "\n"), Level: 2, Title: "2.1 章节标题甲乙"},
	}
	out, got := stripTOC(in, marks)
	if len(got) != 1 {
		t.Fatalf("marks = %d 个, want 1", len(got))
	}
	m := got[0]
	ors := []rune(out)
	if !strings.HasPrefix(string(ors[m.Offset:]), m.Title) {
		t.Fatalf("偏移修正失败：out[%d:] = %q 不以 %q 开头", m.Offset, truncRunesForTOC(string(ors[m.Offset:]), 20), m.Title)
	}
	// 偏移应恰好前移了被删目录行的 rune 数（含换行）
	want := runeLen("正文开头。\n")
	if m.Offset != want {
		t.Fatalf("偏移 = %d, want %d（目录行整行删除）", m.Offset, want)
	}
}

// TestStripTOCMarksNil 空 marks：行为与纯文本剥离完全一致（字节级）。
func TestStripTOCMarksNil(t *testing.T) {
	in := "正文开头。\n" + tocLine("1 智能体软件工程", "3") + "\n正文结尾。"
	a, _ := stripTOC(in, nil)
	b := stripTOCLines(in)
	if a != b {
		t.Fatalf("nil marks 行为漂移：\n%q\nvs\n%q", a, b)
	}
}

// TestStripTOCMarksLeadingIndent mark 偏移指向行首去前导空白处（生成侧
// trimLeftRunes 与剥离侧定位共同维护不变式）。
func TestStripTOCMarksLeadingIndent(t *testing.T) {
	text := "  3.2 缩进标题行甲乙\n正文。"
	marks := []HeadingMark{{Offset: 2, Level: 2, Title: "3.2 缩进标题行甲乙"}}
	out, got := stripTOC(text, marks)
	ors := []rune(out)
	if len(got) != 1 || !strings.HasPrefix(string(ors[got[0].Offset:]), got[0].Title) {
		t.Fatalf("缩进行不变式破坏：%+v", got)
	}
}

func runeLen(s string) int { return len([]rune(s)) }

func truncRunesForTOC(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}
