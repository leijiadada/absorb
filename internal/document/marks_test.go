package document

// F3a/F3c 结构进元数据测试（2026-11-18 评审契约 §7 验收单测；F3b 软边界
// 切块两轮 -5pp 未达标回滚，恢复中点赋值形态）：
//   - 中点规则三案例：标题在块前半/后半/恰中点；
//   - 块边界等价不变式：marks 版与 plain 版块边界/内容逐位一致（结构只进
//     元数据层，正文一个字节不动）；
//   - parseHeadingMarks 往返与容错。

import (
	"encoding/json"
	"strings"
	"testing"

	"absorb-agent/internal/document/extract"
)

// markOf 构造 mark（Path 末位 = Title，与 extract.overlayMarks 产物同形）。
func markOf(offset, level int, path ...string) extract.HeadingMark {
	return extract.HeadingMark{Offset: offset, Level: level, Title: path[len(path)-1], Path: path}
}

// TestAssignMarkSectionPathsMidpoint 中点规则三案例：chunk [0,1000)，mark
// 在 400（前半，mid=500 之后 → 归新章节）、600（后半 → 归前章/空）、500
// （恰中点，≤ 归新章节）。
func TestAssignMarkSectionPathsMidpoint(t *testing.T) {
	const bodyLen = 1000
	body := strings.Repeat("甲", bodyLen)
	// 前 500 rune 无标题行（导语），500~1000 由 mark 覆盖——构造固定边界块：
	// 用 ChunkText 产物验证不现实（断点自适应），直接以合成 pieces 验证规则。
	cases := []struct {
		name   string
		offset int
		want   string
	}{
		{"标题在块前半", 400, "第二章 高级配置"},
		{"标题在块后半", 600, ""},
		{"标题恰中点", 500, "第二章 高级配置"},
	}
	for _, tc := range cases {
		pieces := []textChunk{{Content: body, StartOffset: 0, EndOffset: bodyLen}}
		assignMarkSectionPaths(pieces, []extract.HeadingMark{
			markOf(tc.offset, 1, "第二章 高级配置"),
		})
		if got := pieces[0].SectionPath; got != tc.want {
			t.Fatalf("%s：SectionPath = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestAssignMarkSectionPathsMultiLevel 多级面包屑 + 多 mark 择近：中点越过
// 2 级标题时应取最后一个 mark 的完整链。
func TestAssignMarkSectionPathsMultiLevel(t *testing.T) {
	pieces := []textChunk{{Content: strings.Repeat("乙", 2000), StartOffset: 1000, EndOffset: 3000}}
	marks := []extract.HeadingMark{
		markOf(100, 1, "第一章 总览"),
		markOf(1200, 1, "第二章 高级配置"),
		markOf(1800, 2, "第二章 高级配置", "2.3 知识治理机制"),
	}
	assignMarkSectionPaths(pieces, marks) // mid = 2000 ≥ 1800 → 2.3 的链
	want := "第二章 高级配置 > 2.3 知识治理机制"
	if got := pieces[0].SectionPath; got != want {
		t.Fatalf("SectionPath = %q, want %q", got, want)
	}
}

// TestMarkBreadcrumbTruncation 面包屑截断（v4 同款）：单级 40 rune 补省略号，
// 总长 96 rune 自最老一级丢弃。
func TestMarkBreadcrumbTruncation(t *testing.T) {
	long := strings.Repeat("长", 50)
	got := markBreadcrumb(markOf(0, 2, "首", long, "尾"))
	// 单级截断到 40（39 + …）
	if !strings.Contains(got, strings.Repeat("长", 39)+"…") {
		t.Fatalf("单级截断缺失：%q", truncForMark(got, 60))
	}
	if strings.Contains(got, strings.Repeat("长", 40)) {
		t.Fatalf("单级未截断：%q", truncForMark(got, 60))
	}
	// 总长：构造超长链，最老一级应被丢弃
	parts := []string{strings.Repeat("甲", 45), strings.Repeat("乙", 45), strings.Repeat("丙", 45)}
	got2 := markBreadcrumb(markOf(0, 3, parts...))
	if strings.ContainsRune(got2, '甲') {
		t.Fatalf("总长截断应自最老一级丢弃：%q", truncForMark(got2, 120))
	}
	if len([]rune(got2)) > sectionPathMax {
		t.Fatalf("面包屑超上限：%d", len([]rune(got2)))
	}
}

// TestBuildChunksMarksBoundaryEquivalence 块边界等价不变式：marks 版与
// plain 版的块 Content/StartOffset/EndOffset 逐位一致，仅 SectionPath 不同；
// 且每个 mark.Offset 处原文以 Title 开头（偏移一致性）。
func TestBuildChunksMarksBoundaryEquivalence(t *testing.T) {
	s := &Service{}
	// 合成书：章标题行 + 段落（无 # 前缀，模拟 PDF 纯文本）
	var b strings.Builder
	chap := func(title string, paras int) {
		b.WriteString(title + "\n\n")
		for i := 0; i < paras; i++ {
			b.WriteString(strings.Repeat("正文段落内容，讲述上下文工程与智能体的协作模式。", 4) + "\n\n")
		}
	}
	chap("第一章 需求工程概述", 10)
	chap("第二章 结构化知识抽取", 10)
	chap("第三章 检索增强生成", 10)
	text := b.String()

	// marks 偏移直接从文本反查（模拟 extract 产物：指向标题行首）
	var marks []extract.HeadingMark
	off := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "第") && strings.Contains(line, "章 ") {
			marks = append(marks, markOf(off, 1, line))
		}
		off += len([]rune(line)) + 1
	}
	ors := []rune(text)
	for _, m := range marks { // Offset 为 rune 口径（与 chunk Start/EndOffset 同口径）
		if !strings.HasPrefix(string(ors[m.Offset:]), m.Title) {
			t.Fatalf("mark 偏移不变式破坏：%q @ %d", m.Title, m.Offset)
		}
	}

	plain, err := s.buildChunks(text, nil)
	if err != nil {
		t.Fatalf("plain buildChunks: %v", err)
	}
	marked, err := s.buildChunks(text, marks)
	if err != nil {
		t.Fatalf("marked buildChunks: %v", err)
	}
	if len(plain) != len(marked) {
		t.Fatalf("chunk 数不一致：plain %d vs marks %d", len(plain), len(marked))
	}
	for i := range plain {
		p, m := plain[i], marked[i]
		if p.Content != m.Content || p.StartOffset != m.StartOffset || p.EndOffset != m.EndOffset {
			t.Fatalf("块 %d 边界/内容漂移：plain[%d,%d] vs marks[%d,%d]",
				i, p.StartOffset, p.EndOffset, m.StartOffset, m.EndOffset)
		}
		if p.SearchText != m.SearchText {
			t.Fatalf("块 %d search_text 漂移", i)
		}
	}
	// marks 版应有非空面包屑且与 mark 链一致
	covered := 0
	for _, c := range marked {
		if c.SectionPath != "" {
			covered++
		}
	}
	cover := float64(covered) / float64(len(marked))
	if cover < 0.99 {
		t.Fatalf("面包屑覆盖 %.2f%% < 99%%", cover*100)
	}
}

// TestParseHeadingMarksRoundTrip JSON 往返 + 排序 + 容错。
func TestParseHeadingMarksRoundTrip(t *testing.T) {
	in := []extract.HeadingMark{
		{Offset: 120, Level: 1, Title: "第二章", Path: []string{"第二章"}},
		{Offset: 30, Level: 1, Title: "第一章", Path: []string{"第一章"}},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	got := parseHeadingMarks(string(raw))
	if len(got) != 2 || got[0].Offset != 30 || got[1].Offset != 120 {
		t.Fatalf("往返/排序失败：%+v", got)
	}
	if parseHeadingMarks("") != nil {
		t.Fatal("空串应返回 nil")
	}
	if parseHeadingMarks("   ") != nil {
		t.Fatal("空白串应返回 nil")
	}
	if parseHeadingMarks("not-json{") != nil {
		t.Fatal("坏 JSON 应容错返回 nil")
	}
}

func truncForMark(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}
