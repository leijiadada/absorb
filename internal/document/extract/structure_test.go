package extract

// structure 测试（F3a）：章节树匹配产出 marks 元数据（正文一个字节不动）、
// 模糊匹配/密度门/匹配率门/去重与字符多重集相似度。

import (
	"strings"
	"testing"
)

// 三页合成语料：页 0 两行、页 1 三行、页 2 两行（\f 分页，pdftotext 同款）。
const overlayFixture = "第一章 安装指南\n\n运行 install 脚本完成安装。\f第二章 高级配置\n\n修改 config 文件。\n更多配置说明。\f第三章 附录\n\n术语表见附录 A。"

// markInvariants 断言 marks 偏移不变式：text[Offset:] 以 Title 开头（Offset
// 为 rune 偏移，与 chunk Start/EndOffset 同口径）、面包屑末位等于 Title
// （Path 含自身）、偏移严格递增（赋值二分依赖文档序）。
func markInvariants(t *testing.T, text string, marks []HeadingMark) {
	t.Helper()
	rs := []rune(text)
	for i, m := range marks {
		if !strings.HasPrefix(string(rs[m.Offset:]), m.Title) {
			t.Fatalf("mark[%d] 偏移不变式破坏：text[%d:] = %q 不以 %q 开头", i, m.Offset, truncRunes(string(rs[m.Offset:]), 20), m.Title)
		}
		if n := len(m.Path); n == 0 || m.Path[n-1] != m.Title {
			t.Fatalf("mark[%d] Path 末位应含自身：%v vs %q", i, m.Path, m.Title)
		}
		if i > 0 && m.Offset <= marks[i-1].Offset {
			t.Fatalf("marks 偏移非严格递增：%d then %d", marks[i-1].Offset, m.Offset)
		}
	}
}

func truncRunes(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}

func TestOverlayMarksProducesMetadata(t *testing.T) {
	secs := []docSection{
		{page: 0, level: 1, title: "第一章 安装指南"},
		{page: 1, level: 1, title: "第二章 高级配置"},
		{page: 1, level: 2, title: "2.1 参数说明"},
		{page: 2, level: 1, title: "第三章 附录"},
	}
	marks, n := overlayMarks(overlayFixture, secs, 3)
	if n != 3 { // "2.1 参数说明" 无对应行，跳过
		t.Fatalf("命中 %d 个标题，want 3", n)
	}
	markInvariants(t, overlayFixture, marks)
	// 正文一个字节不动（核心不变式）
	if overlayFixture != "第一章 安装指南\n\n运行 install 脚本完成安装。\f第二章 高级配置\n\n修改 config 文件。\n更多配置说明。\f第三章 附录\n\n术语表见附录 A。" {
		t.Fatal("fixture 被误改（测试自身失效）")
	}
	// 层级栈面包屑：2 级标题的 Path = [父级, 自身]
	want := map[string][]string{
		"第一章 安装指南": {"第一章 安装指南"},
		"第二章 高级配置": {"第二章 高级配置"},
		"第三章 附录":   {"第三章 附录"},
	}
	for _, m := range marks {
		w, ok := want[m.Title]
		if !ok {
			t.Fatalf("意外命中 %q", m.Title)
		}
		if strings.Join(m.Path, "|") != strings.Join(w, "|") {
			t.Fatalf("%q Path = %v, want %v", m.Title, m.Path, w)
		}
	}
}

func TestOverlayMarksHierarchicalPath(t *testing.T) {
	// 语料带 2 级标题行：层级栈应产出 [父, 子] 面包屑
	text := "第二章 高级配置\n\n修改 config 文件。\n2.1 参数说明\n\n参数见下表。"
	secs := []docSection{
		{page: 0, level: 1, title: "第二章 高级配置"},
		{page: 0, level: 2, title: "2.1 参数说明"},
	}
	marks, n := overlayMarks(text, secs, 1)
	if n != 2 {
		t.Fatalf("命中 %d, want 2", n)
	}
	markInvariants(t, text, marks)
	if got := strings.Join(marks[1].Path, " > "); got != "第二章 高级配置 > 2.1 参数说明" {
		t.Fatalf("2 级面包屑 = %q", got)
	}
	if marks[1].Level != 2 || marks[0].Level != 1 {
		t.Fatalf("层级错误：%d/%d", marks[0].Level, marks[1].Level)
	}
}

// TestOverlayMarksScrambledTitle docling 行内乱序标题：字符集不变、顺序
// 打乱（真排列），多重集相似度应满分命中。
func TestOverlayMarksScrambledTitle(t *testing.T) {
	const line = "程序员的自我修养——链接装载与库"
	rs := []rune(line)
	for i, j := 0, len(rs)-1; i < j; i, j = i+1, j-1 { // 真排列：整串倒序
		rs[i], rs[j] = rs[j], rs[i]
	}
	secs := []docSection{{page: 1, level: 1, title: string(rs)}}
	text := "前言内容。\f" + line + "\n\n正文。"
	marks, n := overlayMarks(text, secs, 2)
	if n != 1 {
		t.Fatalf("乱序标题未命中：命中 %d", n)
	}
	markInvariants(t, text, marks)
	if marks[0].Title != line {
		t.Fatalf("Title 应为正文行原文本 %q，得到 %q", line, marks[0].Title)
	}
}

func TestOverlayMarksDensityGates(t *testing.T) {
	// 密度过低（1 标题/100 页）：人月神话式稀疏结构
	sparse := []docSection{{page: 0, level: 1, title: "第一章 安装指南"}}
	if _, n := overlayMarks(strings.Repeat(overlayFixture+"\f", 33), sparse, 100); n != 0 {
		t.Fatal("低密度应弃用")
	}
	// 密度过高（500 标题/100 页）：Go 程序设计语言式伪标题泛滥
	var flood []docSection
	for i := 0; i < 500; i++ {
		flood = append(flood, docSection{page: i % 3, level: 1, title: "第一章 安装指南"})
	}
	if _, n := overlayMarks(overlayFixture, flood, 3); n != 0 {
		t.Fatal("高密度应弃用")
	}
}

func TestOverlayMarksMatchRateGate(t *testing.T) {
	// 10 个标题只命中 1 个（<30%）：整批放弃，避免乱序标题产出伪结构
	var secs []docSection
	for i := 0; i < 9; i++ {
		secs = append(secs, docSection{page: 2, level: 1, title: "不存在的章节标题甲乙丙"})
	}
	secs = append(secs, docSection{page: 0, level: 1, title: "第一章 安装指南"})
	if _, n := overlayMarks(overlayFixture, secs, 3); n != 0 {
		t.Fatal("低匹配率应弃用")
	}
}

func TestOverlayMarksGarbledAndDedup(t *testing.T) {
	// 乱码标题跳过 + 同行去重（docling 多行拆分标题两段指向同一行时只命中一次）
	secs := []docSection{
		{page: 0, level: 1, title: "第\uFFFD章 乱码标题甲乙"},
		{page: 0, level: 1, title: "第一章 安装指南"},
		{page: 0, level: 2, title: "安装指南第一章指引"}, // 只会匹配已被占用的同一行 → 去重
	}
	marks, n := overlayMarks(overlayFixture, secs, 3)
	if n != 1 { // 乱码跳过 + 去重后仅 1 个命中
		t.Fatalf("命中 %d，want 1", n)
	}
	markInvariants(t, overlayFixture, marks)
	if marks[0].Title != "第一章 安装指南" {
		t.Fatalf("命中标题 %q，want 第一章 安装指南", marks[0].Title)
	}
}

// TestOverlayMarksTOCLineExcluded 连续文本模式：目录行（点引导线）排除。
func TestOverlayMarksTOCLineExcluded(t *testing.T) {
	flat := strings.ReplaceAll(overlayFixture, "\f", "\n\n")
	flat += "\n目录\n第一章 安装指南............1\n第二章 高级配置............5\n"
	secs := []docSection{
		{page: 0, level: 1, title: "第一章 安装指南"},
		{page: 1, level: 1, title: "第二章 高级配置"},
	}
	marks, n := overlayMarks(flat, secs, 3)
	if n != 2 {
		t.Fatalf("连续文本命中 %d，want 2（目录行应被排除）", n)
	}
	markInvariants(t, flat, marks)
	// 命中的必须是正文位置的行，不是目录行（目录行偏移远在文末）
	for _, m := range marks {
		if strings.Contains(m.Title, "....") {
			t.Fatalf("标题匹配到了目录行：%q @ %d", m.Title, m.Offset)
		}
	}
}

func TestCharMultisetSim(t *testing.T) {
	if got := charMultisetSim("智能体软件工程", "工程软件体智能"); got < 0.99 {
		t.Fatalf("乱序同字符应 ≈1：%.2f", got)
	}
	if got := charMultisetSim("智能体软件工程", "智能体软件工程 "); got < 0.99 {
		t.Fatalf("空格应忽略：%.2f", got)
	}
	if got := charMultisetSim("智能体", "数据库"); got > 0.1 {
		t.Fatalf("无关联应 ≈0：%.2f", got)
	}
}
