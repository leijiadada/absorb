package sqlite

// documents 表（M2.5）：URL 文档锚点 upsert 幂等、标题刷新、摘要覆盖写、scope 隔离。

import (
	"context"
	"testing"
	"time"

	"absorb-agent/internal/store/storage"
)

func TestDocumentsUpsertAndSummary(t *testing.T) {
	st, _ := openStore(t, "documents.db")
	ctx := context.Background()

	// 首次 upsert：新建锚点
	doc, err := st.UpsertDocument(ctx, "app", "user", "https://example.invalid/wal", "SQLite WAL")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if doc.ID == "" || doc.Title != "SQLite WAL" || doc.URL != "https://example.invalid/wal" {
		t.Fatalf("doc mismatch: %+v", doc)
	}

	// 同 URL 重复 upsert：同锚点 + 标题变化刷新
	again, err := st.UpsertDocument(ctx, "app", "user", "https://example.invalid/wal", "SQLite WAL 指南")
	if err != nil {
		t.Fatalf("upsert again: %v", err)
	}
	if again.ID != doc.ID {
		t.Fatalf("upsert again id = %s, want %s", again.ID, doc.ID)
	}
	if again.Title != "SQLite WAL 指南" {
		t.Fatalf("title not refreshed: %q", again.Title)
	}

	// 摘要覆盖写（M2.5 层次A：同锚点只保留最新总结）
	if err := st.UpdateDocumentSummary(ctx, "app", "user", doc.ID, "第一版总结", "s1", "r1"); err != nil {
		t.Fatalf("update summary: %v", err)
	}
	if err := st.UpdateDocumentSummary(ctx, "app", "user", doc.ID, "第二版总结", "s1", "r2"); err != nil {
		t.Fatalf("update summary again: %v", err)
	}
	got, err := st.UpsertDocument(ctx, "app", "user", "https://example.invalid/wal", "SQLite WAL 指南")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.SummaryText != "第二版总结" || got.SummarySessionID != "s1" || got.SummaryEventID != "r2" {
		t.Fatalf("summary mismatch: %+v", got)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("timestamps missing: %+v", got)
	}

	// 不存在的文档更新摘要：报错（调用方 service 层先 upsert，正常流不会走到）
	if err := st.UpdateDocumentSummary(ctx, "app", "user", "no-such-doc", "t", "s1", "r1"); err == nil {
		t.Fatal("update missing document should fail")
	}

	// scope 隔离：不同 user 同 URL 各自独立锚点
	other, err := st.UpsertDocument(ctx, "app", "user-b", "https://example.invalid/wal", "别人的 WALE")
	if err != nil {
		t.Fatalf("upsert other user: %v", err)
	}
	if other.ID == doc.ID {
		t.Fatal("different users must not share document anchor")
	}
}

func TestDocumentContentSaveLoad(t *testing.T) {
	st, _ := openStore(t, "document_contents.db")
	ctx := context.Background()
	url := "https://example.invalid/paper.pdf"

	// 查无：nil, nil（宁缺勿错，调用方按未准备处理）
	if got, err := st.LoadDocumentContent(ctx, "app", "user", url); err != nil || got != nil {
		t.Fatalf("load missing = (%v, %v), want (nil, nil)", got, err)
	}

	dc := &storage.DocumentContent{
		URL:               url,
		ContentText:       "第一版正文",
		CharCount:         5,
		ContentType:       "application/pdf",
		ByteSize:          15,
		ContentHash:       "hash-1",
		ExtractionStatus:  "ready",
		ExtractionVersion: 1,
		Truncated:         false,
		FetchedAt:         time.Unix(1700000000, 0),
	}
	if err := st.SaveDocumentContent(ctx, "app", "user", dc); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := st.LoadDocumentContent(ctx, "app", "user", url)
	if err != nil || got == nil {
		t.Fatalf("load: (%v, %v)", got, err)
	}
	if got.ContentText != dc.ContentText || got.CharCount != dc.CharCount ||
		got.ContentType != dc.ContentType || got.ByteSize != dc.ByteSize ||
		got.ContentHash != dc.ContentHash || got.ExtractionStatus != dc.ExtractionStatus ||
		got.ExtractionVersion != dc.ExtractionVersion || got.Truncated != dc.Truncated ||
		!got.FetchedAt.Equal(dc.FetchedAt) || got.UpdatedAt.IsZero() {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}

	// 重新准备：同 (app,user,url) 覆盖写
	dc2 := &storage.DocumentContent{
		URL: url, ContentText: "第二版正文（重新提取）", CharCount: 11,
		ContentType: "application/pdf", ExtractionStatus: "partial",
		ExtractionVersion: 1, Truncated: true, FetchedAt: time.Unix(1700000001, 0),
	}
	if err := st.SaveDocumentContent(ctx, "app", "user", dc2); err != nil {
		t.Fatalf("save again: %v", err)
	}
	got, err = st.LoadDocumentContent(ctx, "app", "user", url)
	if err != nil || got == nil {
		t.Fatalf("load again: (%v, %v)", got, err)
	}
	if got.ContentText != dc2.ContentText || got.ExtractionStatus != "partial" || !got.Truncated {
		t.Fatalf("overwrite mismatch: %+v", got)
	}

	// 归属隔离：不同 user / app 查无
	if got, err := st.LoadDocumentContent(ctx, "app", "user-b", url); err != nil || got != nil {
		t.Fatalf("other user should see nothing, got (%v, %v)", got, err)
	}
	if got, err := st.LoadDocumentContent(ctx, "app-b", "user", url); err != nil || got != nil {
		t.Fatalf("other app should see nothing, got (%v, %v)", got, err)
	}

	// nil 入参防御
	if err := st.SaveDocumentContent(ctx, "app", "user", nil); err == nil {
		t.Fatal("save nil content should fail")
	}
}
