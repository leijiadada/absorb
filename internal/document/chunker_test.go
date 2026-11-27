package document

// chunker（M1）：偏移不变量（text[start:end]==content）、覆盖完整、重叠、碎块合并、
// 断句优先级、标题面包屑、围栏保护区、重叠吸附。

import (
	"fmt"
	"strings"
	"testing"
)

// TestChunkTextOffsetInvariant 核心不变量：每个块 content 必须与原文切片一致。
func TestChunkTextOffsetInvariant(t *testing.T) {
	text := strings.Repeat("智能体工程改变软件研发模式。这是第二句。第三句更长一些，用来拉开窗口距离。\n\n", 120)
	rs := []rune(text)
	for _, c := range ChunkText(text) {
		if got := string(rs[c.StartOffset:c.EndOffset]); got != c.Content {
			t.Fatalf("offset invariant broken: [%d:%d]", c.StartOffset, c.EndOffset)
		}
	}
}

// TestChunkTextCoversAllText 无遗漏：首块从头开始、相邻块连续（允许重叠回退）、并集覆盖全文。
func TestChunkTextCoversAllText(t *testing.T) {
	text := strings.Repeat("段落一的内容。\n段落二的内容。\n\n", 200)
	chunks := ChunkText(text)
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d, want multiple", len(chunks))
	}
	if chunks[0].StartOffset != 0 {
		t.Fatalf("first chunk start = %d, want 0", chunks[0].StartOffset)
	}
	last := chunks[len(chunks)-1]
	total := len([]rune(text))
	if last.EndOffset != total {
		t.Fatalf("last chunk end = %d, want %d", last.EndOffset, total)
	}
	for i := 1; i < len(chunks); i++ {
		if chunks[i].StartOffset > chunks[i-1].EndOffset {
			t.Fatalf("gap between chunk %d and %d: %d > %d", i-1, i, chunks[i].StartOffset, chunks[i-1].EndOffset)
		}
		if chunks[i].StartOffset <= chunks[i-1].StartOffset {
			t.Fatalf("chunk %d not advancing: start %d <= %d", i, chunks[i].StartOffset, chunks[i-1].StartOffset)
		}
	}
}

// TestChunkTextPrefersParagraphBreak 断句优先级：窗口内有空行时在空行处切。
func TestChunkTextPrefersParagraphBreak(t *testing.T) {
	var b strings.Builder
	b.WriteString(strings.Repeat("甲", 800)) // 段落边界落在目标窗口（900）的断点回看区内
	b.WriteString("\n\n")
	b.WriteString(strings.Repeat("乙", 2000))
	chunks := ChunkText(b.String())
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d", len(chunks))
	}
	first := chunks[0]
	if !strings.HasSuffix(first.Content, "\n\n") || strings.Contains(first.Content, "乙") {
		t.Fatalf("break should fall on paragraph boundary, got suffix %q", first.Content[max(0, len(first.Content)-8):])
	}
}

// TestChunkTextPrefersLineBreakOverSentence 断句优先级：窗口内单换行
// 优先于更靠右的句末标点。
func TestChunkTextPrefersLineBreakOverSentence(t *testing.T) {
	var b strings.Builder
	b.WriteString(strings.Repeat("正文一句。", 160)) // 0..799，句末标点密集
	b.WriteString("\n")                         // 800：单换行
	b.WriteString(strings.Repeat("补充说明。", 20))  // 801..900，更靠近窗口末端的句末标点
	chunks := ChunkText(b.String())
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d", len(chunks))
	}
	first := chunks[0]
	if first.EndOffset != 801 || !strings.HasSuffix(first.Content, "\n") {
		t.Fatalf("break should fall on line break at 801, got end=%d suffix=%q",
			first.EndOffset, first.Content[max(0, len(first.Content)-8):])
	}
}

// TestChunkTextPrefersBlankLineOverCloserBreaks 断句优先级：空行即使比
// 单换行/句末标点更远，仍然胜出（段落完整性 > 块长均匀性）。
func TestChunkTextPrefersBlankLineOverCloserBreaks(t *testing.T) {
	var b strings.Builder
	b.WriteString(strings.Repeat("甲", 700))
	b.WriteString("\n\n")                    // 700..701：空行（段落边界）
	b.WriteString(strings.Repeat("乙", 150))  // 702..851
	b.WriteString("\n")                      // 852：更近的单换行
	b.WriteString(strings.Repeat("丙句。", 12)) // 853..888：更近的句末标点
	b.WriteString(strings.Repeat("丁", 12))   // 889..900，撑出第二个窗口
	chunks := ChunkText(b.String())
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d", len(chunks))
	}
	first := chunks[0]
	if first.EndOffset != 702 || !strings.HasSuffix(first.Content, "\n\n") {
		t.Fatalf("blank line at 700 should win, got end=%d suffix=%q",
			first.EndOffset, first.Content[max(0, len(first.Content)-8):])
	}
}

// TestChunkTextBlankLineRunFullyConsumed 连续多个 '\n' 组成的空行 run
// 整体归入当前块，不留残留在下一块开头。
func TestChunkTextBlankLineRunFullyConsumed(t *testing.T) {
	var b strings.Builder
	b.WriteString(strings.Repeat("甲", 880))
	b.WriteString("\n\n\n") // 880..882：三连换行
	b.WriteString(strings.Repeat("乙", 30))
	chunks := ChunkText(b.String())
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d", len(chunks))
	}
	first, second := chunks[0], chunks[1]
	if first.EndOffset != 883 || !strings.HasSuffix(first.Content, "\n\n\n") {
		t.Fatalf("whole newline run should stay in first chunk, got end=%d suffix=%q",
			first.EndOffset, first.Content[max(0, len(first.Content)-8):])
	}
	if second.StartOffset != first.EndOffset-chunkOverlapRunes {
		t.Fatalf("overlap start mismatch: second.StartOffset=%d want %d",
			second.StartOffset, first.EndOffset-chunkOverlapRunes)
	}
}

// TestChunkTextTinyTailMerged 末尾碎块并入前块。
func TestChunkTextTinyTailMerged(t *testing.T) {
	var b strings.Builder
	b.WriteString(strings.Repeat("正文内容。", 500)) // ~2500 runes，切成多块
	b.WriteString("尾。")                         // 碎尾
	chunks := ChunkText(b.String())
	last := chunks[len(chunks)-1]
	if last.EndOffset-last.StartOffset < minChunkRunes && len(chunks) > 1 {
		t.Fatalf("tiny tail not merged: last len=%d", last.EndOffset-last.StartOffset)
	}
}

// TestChunkTextShortText 短文本单块、空文本零块。
func TestChunkTextShortText(t *testing.T) {
	if got := ChunkText(""); len(got) != 0 {
		t.Fatalf("empty text chunks = %d", len(got))
	}
	one := ChunkText("短文。")
	if len(one) != 1 || one[0].StartOffset != 0 || one[0].EndOffset != 3 {
		t.Fatalf("short text mismatch: %+v", one)
	}
}

// checkInvariants 通用断言：偏移不变式 + 首块从头 + 单调前进 + 覆盖到文末。
func checkInvariants(t *testing.T, text string, chunks []textChunk) {
	t.Helper()
	rs := []rune(text)
	if len(chunks) == 0 {
		return
	}
	if chunks[0].StartOffset != 0 {
		t.Fatalf("first chunk start = %d, want 0", chunks[0].StartOffset)
	}
	if chunks[len(chunks)-1].EndOffset != len(rs) {
		t.Fatalf("last chunk end = %d, want %d", chunks[len(chunks)-1].EndOffset, len(rs))
	}
	for i, c := range chunks {
		if got := string(rs[c.StartOffset:c.EndOffset]); got != c.Content {
			t.Fatalf("chunk %d offset invariant broken: [%d:%d]", i, c.StartOffset, c.EndOffset)
		}
		if i > 0 && (c.StartOffset > chunks[i-1].EndOffset || c.StartOffset <= chunks[i-1].StartOffset) {
			t.Fatalf("chunk %d not advancing/overlapping correctly: %d (prev %d..%d)",
				i, c.StartOffset, chunks[i-1].StartOffset, chunks[i-1].EndOffset)
		}
	}
}

// TestChunkTextHeadingSections 标题感知（v4）：目标块内的分区整段成块，
// SectionPath 记录标题面包屑。
func TestChunkTextHeadingSections(t *testing.T) {
	var b strings.Builder
	b.WriteString("# 部署指南\n\n")
	b.WriteString(strings.Repeat("安装章节的正文内容。", 60)) // ~600 runes
	b.WriteString("\n\n## 高级配置\n\n")
	b.WriteString(strings.Repeat("配置章节的正文内容。", 60)) // ~600 runes，两节合计 >900 不整包
	text := b.String()
	chunks := ChunkText(text)
	checkInvariants(t, text, chunks)
	if len(chunks) != 2 {
		t.Fatalf("chunks = %d, want 2 sections as whole chunks", len(chunks))
	}
	if chunks[0].SectionPath != "部署指南" {
		t.Fatalf("chunk0 path = %q, want %q", chunks[0].SectionPath, "部署指南")
	}
	if chunks[1].SectionPath != "部署指南 > 高级配置" {
		t.Fatalf("chunk1 path = %q, want %q", chunks[1].SectionPath, "部署指南 > 高级配置")
	}
	if !strings.Contains(chunks[0].Content, "安装章节") || !strings.Contains(chunks[1].Content, "配置章节") {
		t.Fatalf("section content misplaced")
	}
}

// TestChunkTextOversizedSectionWindowSplit 超长分区退回滑动窗口：各块共享
// 分区面包屑，块间不跨分区边界。
func TestChunkTextOversizedSectionWindowSplit(t *testing.T) {
	var b strings.Builder
	b.WriteString("# 手册\n\n")
	b.WriteString(strings.Repeat("长章节连续正文没有小标题只有句子。", 140)) // ~2240 runes
	text := b.String()
	chunks := ChunkText(text)
	checkInvariants(t, text, chunks)
	if len(chunks) < 3 {
		t.Fatalf("chunks = %d, want window split of oversized section", len(chunks))
	}
	for k, c := range chunks {
		if c.SectionPath != "手册" {
			t.Fatalf("chunk %d path = %q, want %q", k, c.SectionPath, "手册")
		}
	}
}

// TestChunkTextFenceProtected 围栏保护区：围栏内 # 注释行不误判标题；
// 围栏内部的块边界只能落在换行之后（不在代码行中间切割）。
func TestChunkTextFenceProtected(t *testing.T) {
	var b strings.Builder
	b.WriteString("# Shell 指南\n\n```bash\n")
	for i := 0; i < 30; i++ {
		b.WriteString(fmt.Sprintf("# step %d: echo run_%d\n", i, i)) // 围栏内 # 行
	}
	b.WriteString("```\n\n## 结尾\n\n这是结尾段落的正文内容。")
	text := b.String()
	rs := []rune(text)

	headings, fences := scanStructure(rs)
	if len(headings) != 2 {
		t.Fatalf("headings = %d, want 2（围栏内 # 不误判）", len(headings))
	}
	if len(fences) != 1 {
		t.Fatalf("fences = %d, want 1", len(fences))
	}

	chunks := ChunkText(text)
	checkInvariants(t, text, chunks)
	for _, c := range chunks {
		// 块起点严格落在围栏内部（不含围栏首行）时，必须紧跟换行之后
		if c.StartOffset > fences[0].start && c.StartOffset < fences[0].end &&
			rs[c.StartOffset-1] != '\n' {
			t.Fatalf("chunk starts mid-line inside fence at %d", c.StartOffset)
		}
	}
}

// TestChunkTextNoHeadingsNoPath 无标题纯文本（PDF 提取产物）降级路径：
// 行为与 v3 一致，全部块无面包屑。
func TestChunkTextNoHeadingsNoPath(t *testing.T) {
	text := strings.Repeat("纯文本正文没有标题行。", 120)
	chunks := ChunkText(text)
	checkInvariants(t, text, chunks)
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d, want multiple", len(chunks))
	}
	for k, c := range chunks {
		if c.SectionPath != "" {
			t.Fatalf("chunk %d path = %q, want empty", k, c.SectionPath)
		}
	}
}

// TestChunkTextOverlapSnappedToSentence 重叠起点吸附：落在句中间的重叠起点
// 向前吸附到最近句末标点之后。
func TestChunkTextOverlapSnappedToSentence(t *testing.T) {
	var b strings.Builder
	b.WriteString(strings.Repeat("前导句子。", 120)) // 0..599
	b.WriteString("\n\n")                       // 600..601 段落边界
	b.WriteString(strings.Repeat("正文句子。", 60))  // 602..901
	text := b.String()
	chunks := ChunkText(text)
	checkInvariants(t, text, chunks)
	if len(chunks) != 2 {
		t.Fatalf("chunks = %d, want 2", len(chunks))
	}
	if chunks[0].EndOffset != 602 {
		t.Fatalf("first break = %d, want 602", chunks[0].EndOffset)
	}
	// brk-180=422 落在句中（rs[422]='句'），吸附到 424 的句号之后 = 425
	if chunks[1].StartOffset != 425 || !strings.HasPrefix(chunks[1].Content, "前导句子。") {
		t.Fatalf("overlap start = %d, want 425（句末边界）", chunks[1].StartOffset)
	}
}
