package document

// chunker（M1）：偏移不变量（text[start:end]==content）、覆盖完整、重叠、碎块合并、断句优先级。

import (
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
	b.WriteString(strings.Repeat("甲", 380)) // 段落边界落在目标窗口（400）末端附近
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

// TestChunkTextTinyTailMerged 末尾碎块并入前块。
func TestChunkTextTinyTailMerged(t *testing.T) {
	var b strings.Builder
	b.WriteString(strings.Repeat("正文内容。", 500)) // ~2500 runes，切成多块
	b.WriteString("尾。")                  // 碎尾
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
