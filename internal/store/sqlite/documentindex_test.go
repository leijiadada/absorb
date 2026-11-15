package sqlite

// document_index 三件套（M1）：原子替换、FTS+bm25 检索、租户/范围隔离、失败标记。

import (
	"context"
	"strings"
	"testing"

	"absorb-agent/internal/store/storage"
)

func idxTestChunks(doc string, n int) []*storage.DocumentChunk {
	out := make([]*storage.DocumentChunk, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, &storage.DocumentChunk{
			DocumentID:       doc,
			ChunkIndex:       i,
			Content:          "第" + string(rune('A'+i)) + "块：软件工程改变传统研发模式",
			SearchText:       "软件工程 改变 传统 研发 模式",
			StartOffset:      i * 100,
			EndOffset:        i*100 + 50,
			TokenCount:       50,
			ContentHash:      "h",
			ChunkerVersion:   1,
			TokenizerVersion: "t1",
		})
	}
	return out
}

func TestReplaceDocumentIndexRoundtrip(t *testing.T) {
	st, _ := openStore(t, "docindex.db")
	ctx := context.Background()
	doc, _ := st.UpsertDocument(ctx, "app", "user", "https://x.invalid/a.pdf", "测试手册")

	// 首次建索引：3 块 + ready
	st.ReplaceDocumentIndex(ctx, "app", "user", doc.ID, idxTestChunks(doc.ID, 3), &storage.DocumentIndexState{
		DocumentID: doc.ID, IndexedContentHash: "hash1", IndexStatus: storage.IndexStatusReady,
		ChunkerVersion: 1, TokenizerVersion: "t1", ChunkCount: 3,
	})
	state, err := st.LoadDocumentIndexState(ctx, "app", "user", doc.ID)
	if err != nil || state == nil {
		t.Fatalf("load state: (%v, %v)", state, err)
	}
	if state.IndexStatus != storage.IndexStatusReady || state.ChunkCount != 3 || state.IndexedContentHash != "hash1" {
		t.Fatalf("state mismatch: %+v", state)
	}

	// 重建（2 块）：整体替换，不残留旧块
	st.ReplaceDocumentIndex(ctx, "app", "user", doc.ID, idxTestChunks(doc.ID, 2), &storage.DocumentIndexState{
		DocumentID: doc.ID, IndexedContentHash: "hash2", IndexStatus: storage.IndexStatusReady,
		ChunkerVersion: 1, TokenizerVersion: "t1", ChunkCount: 2,
	})
	hits, err := st.SearchDocumentChunks(ctx, "app", "user", `"软件工程"`, []string{doc.ID}, 50)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want 2（旧块必须被整体替换）", len(hits))
	}
	state, _ = st.LoadDocumentIndexState(ctx, "app", "user", doc.ID)
	if state.IndexedContentHash != "hash2" || state.ChunkCount != 2 {
		t.Fatalf("state not replaced: %+v", state)
	}
}

func TestReplaceDocumentIndexFailureMarkOnly(t *testing.T) {
	st, _ := openStore(t, "docindex-fail.db")
	ctx := context.Background()
	doc, _ := st.UpsertDocument(ctx, "app", "user", "https://x.invalid/b.pdf", "失败标记")
	st.ReplaceDocumentIndex(ctx, "app", "user", doc.ID, idxTestChunks(doc.ID, 2), &storage.DocumentIndexState{
		DocumentID: doc.ID, IndexStatus: storage.IndexStatusReady, ChunkCount: 2,
	})
	// chunks=nil：只覆盖状态，不触碰既有索引（构建失败保留旧索引语义）
	st.ReplaceDocumentIndex(ctx, "app", "user", doc.ID, nil, &storage.DocumentIndexState{
		DocumentID: doc.ID, IndexStatus: storage.IndexStatusFailed, ErrorMessage: "boom",
	})
	state, _ := st.LoadDocumentIndexState(ctx, "app", "user", doc.ID)
	if state.IndexStatus != storage.IndexStatusFailed || state.ErrorMessage != "boom" {
		t.Fatalf("failed mark mismatch: %+v", state)
	}
	hits, _ := st.SearchDocumentChunks(ctx, "app", "user", `"软件工程"`, []string{doc.ID}, 50)
	if len(hits) != 2 {
		t.Fatalf("old index must survive failure mark, hits=%d", len(hits))
	}
}

func TestSearchDocumentChunksScopes(t *testing.T) {
	st, _ := openStore(t, "docindex-scope.db")
	ctx := context.Background()
	d1, _ := st.UpsertDocument(ctx, "app", "user", "https://x.invalid/1.pdf", "文档一")
	d2, _ := st.UpsertDocument(ctx, "app", "user", "https://x.invalid/2.pdf", "文档二")
	for _, d := range []string{d1.ID, d2.ID} {
		st.ReplaceDocumentIndex(ctx, "app", "user", d, idxTestChunks(d, 2), &storage.DocumentIndexState{
			DocumentID: d, IndexStatus: storage.IndexStatusReady, ChunkCount: 2,
		})
	}
	// 其他 user 同 URL 不可见（租户隔离）
	st.UpsertDocument(ctx, "app", "user-b", "https://x.invalid/1.pdf", "他人文档")
	st.ReplaceDocumentIndex(ctx, "app", "user-b", d1.ID, idxTestChunks(d1.ID, 2), &storage.DocumentIndexState{
		DocumentID: d1.ID, IndexStatus: storage.IndexStatusReady, ChunkCount: 2,
	})

	// 全部文档范围：只含当前 user 的 4 块
	hits, err := st.SearchDocumentChunks(ctx, "app", "user", `"软件工程"`, nil, 50)
	if err != nil {
		t.Fatalf("search all: %v", err)
	}
	if len(hits) != 4 {
		t.Fatalf("hits = %d, want 4", len(hits))
	}
	// 单文档范围
	hits, _ = st.SearchDocumentChunks(ctx, "app", "user", `"软件工程"`, []string{d1.ID}, 50)
	if len(hits) != 2 || hits[0].Title != "文档一" {
		t.Fatalf("scoped hits = %d, want 2 of 文档一", len(hits))
	}
	// 空 query：返回空不报错
	if hits, _ = st.SearchDocumentChunks(ctx, "app", "user", "   ", nil, 8); len(hits) != 0 {
		t.Fatalf("empty query must return no hits, got %d", len(hits))
	}
	// limit 生效
	if hits, _ = st.SearchDocumentChunks(ctx, "app", "user", `"软件工程"`, nil, 3); len(hits) != 3 {
		t.Fatalf("limit hits = %d, want 3", len(hits))
	}
}

func TestSearchDocumentChunksRanking(t *testing.T) {
	st, _ := openStore(t, "docindex-rank.db")
	ctx := context.Background()
	doc, _ := st.UpsertDocument(ctx, "app", "user", "https://x.invalid/r.pdf", "排序验证")
	// 三块：标题命中块应排在纯正文命中之前（title 权重 4.0）
	// 注：title 来自 documents 表（"排序验证"），块内容分别含不同密度词项
	chunks := []*storage.DocumentChunk{
		{DocumentID: doc.ID, ChunkIndex: 0, Content: "偶然提到检索一次", SearchText: "偶然 提到 检索 一次", StartOffset: 0, EndOffset: 8},
		{DocumentID: doc.ID, ChunkIndex: 1, Content: "检索 检索 检索 全文检索与排序", SearchText: "检索 检索 检索 全文检索 排序", StartOffset: 10, EndOffset: 30},
	}
	st.ReplaceDocumentIndex(ctx, "app", "user", doc.ID, chunks, &storage.DocumentIndexState{
		DocumentID: doc.ID, IndexStatus: storage.IndexStatusReady, ChunkCount: 2,
	})
	hits, err := st.SearchDocumentChunks(ctx, "app", "user", `"检索"`, []string{doc.ID}, 10)
	if err != nil || len(hits) != 2 {
		t.Fatalf("search: hits=%d err=%v", len(hits), err)
	}
	// 词频高的块 bm25 更小（更相关），排在前
	if !strings.Contains(hits[0].Content, "全文检索") {
		t.Fatalf("rank mismatch: first=%q want dense chunk", hits[0].Content)
	}
	if hits[0].Score > hits[1].Score {
		t.Fatalf("score must ascend (bm25 smaller=better): %v > %v", hits[0].Score, hits[1].Score)
	}
}
