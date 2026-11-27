package document_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"absorb-agent/internal/document"
	"absorb-agent/internal/document/extract"
)

// 存量文档章节导航原地刷新（撤销门控 v7）不变式：正文/块边界/chunk_id/向量
// 逐位不变，仅 section_path 两列更新；marks 为空时不误清空既有面包屑。
func TestSectionPathsRefreshInPlace(t *testing.T) {
	st, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()
	const (
		app, user = "app", "user"
	)

	// 合成正文：导语 + 两个标题分区，足够长切出多块。
	body := "导语区" + strings.Repeat("甲", 1000) + "\n" +
		"第一章 总览\n" + strings.Repeat("乙", 1000) + "\n" +
		"2.3 知识治理机制\n" + strings.Repeat("丙", 1000)
	url := "https://x.invalid/nav-refresh.pdf"
	docID := idxContent(t, st, ctx, url, body)

	// 第一次：无 marks 建索引（模拟门控下的旧文档）
	svc := document.NewService(st, nil, app, user)
	svc.BuildIndex(ctx, docID, mustContent(t, st, ctx, url))
	before, err := st.LoadDocumentChunks(ctx, app, user, docID)
	if err != nil || len(before) == 0 {
		t.Fatalf("load chunks before: (%v, %d)", err, len(before))
	}
	for _, c := range before {
		if c.SectionPath != "" {
			t.Fatalf("旧文档应无面包屑: %q", c.SectionPath)
		}
	}
	idsBefore := chunkIDs(t, st, ctx, docID)

	// 第二次：给正文缓存落 marks（模拟撤销门控后重新 prepare 产出），重跑 BuildIndex。
	// HeadingMark.Offset 是 rune 偏移，须用前缀 rune 数而非 strings.Index 的字节偏移。
	runeOff := func(title string) int {
		return len([]rune(body[:strings.Index(body, title)]))
	}
	marks := []extract.HeadingMark{
		{Offset: runeOff("第一章 总览"), Level: 1, Title: "第一章 总览", Path: []string{"第一章 总览"}},
		{Offset: runeOff("2.3 知识治理机制"), Level: 2, Title: "2.3 知识治理机制",
			Path: []string{"第一章 总览", "2.3 知识治理机制"}},
	}
	mj, _ := json.Marshal(marks)
	dc := mustContent(t, st, ctx, url)
	dc.HeadingMarks = string(mj)
	if err := st.SaveDocumentContent(ctx, app, user, dc); err != nil {
		t.Fatalf("save marks: %v", err)
	}
	svc.BuildIndex(ctx, docID, mustContent(t, st, ctx, url))

	after, err := st.LoadDocumentChunks(ctx, app, user, docID)
	if err != nil {
		t.Fatalf("load chunks after: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("chunk 数漂移: %d -> %d", len(before), len(after))
	}
	covered := 0
	for i, c := range after {
		if c.ID != before[i].ID {
			t.Fatalf("chunk_id 被改写 (chunk %d)", i)
		}
		if c.Content != before[i].Content || c.StartOffset != before[i].StartOffset ||
			c.EndOffset != before[i].EndOffset || c.SearchText != before[i].SearchText {
			t.Fatalf("正文/偏移/分词被改写 (chunk %d)", i)
		}
		if c.SectionPath != "" {
			covered++
		}
	}
	if covered == 0 {
		t.Fatal("原地刷新后全部无面包屑，预期至少部分块获得章节路径")
	}
	idsAfter := chunkIDs(t, st, ctx, docID)
	for i := range idsBefore {
		if idsBefore[i] != idsAfter[i] {
			t.Fatalf("chunk_id 列表漂移 (chunk %d)", i)
		}
	}

	// marks 清空后再 prepare：不应把已有面包屑清掉（防误清空）。
	dc = mustContent(t, st, ctx, url)
	dc.HeadingMarks = ""
	if err := st.SaveDocumentContent(ctx, app, user, dc); err != nil {
		t.Fatalf("clear marks: %v", err)
	}
	svc.BuildIndex(ctx, docID, mustContent(t, st, ctx, url))
	cleared, _ := st.LoadDocumentChunks(ctx, app, user, docID)
	for i, c := range cleared {
		if c.SectionPath != after[i].SectionPath {
			t.Fatalf("marks 清空误改了已有面包屑 (chunk %d): %q -> %q", i, after[i].SectionPath, c.SectionPath)
		}
	}
}
