package sqlite

// chunk 向量存取测试（M3）：BLOB 编解码回环、文档级替换、model 过滤、JOIN 命中。

import (
	"context"
	"testing"

	"absorb-agent/internal/store/storage"
)

func vecRows(doc string, n, dims int) []*storage.ChunkVectorRow {
	out := make([]*storage.ChunkVectorRow, 0, n)
	for i := 0; i < n; i++ {
		v := make([]float32, dims)
		v[0] = float32(i + 1)
		out = append(out, &storage.ChunkVectorRow{
			DocumentID: doc, ChunkIndex: i, Dims: dims, Vector: v,
		})
	}
	return out
}

func TestChunkVectorsRoundtrip(t *testing.T) {
	st, _ := openStore(t, "chunkvec.db")
	ctx := context.Background()
	doc, _ := st.UpsertDocument(ctx, "app", "user", "https://x.invalid/v.pdf", "向量书")
	st.ReplaceDocumentIndex(ctx, "app", "user", doc.ID, idxTestChunks(doc.ID, 3), &storage.DocumentIndexState{
		DocumentID: doc.ID, IndexStatus: storage.IndexStatusReady, ChunkCount: 3,
	})

	// 写 3 条向量（model m1）
	rows := vecRows(doc.ID, 3, 4)
	st.ReplaceDocumentVectors(ctx, "app", "user", doc.ID, "m1", rows)
	got, err := st.LoadDocumentVectors(ctx, "app", "user", doc.ID, "m1")
	if err != nil || len(got) != 3 {
		t.Fatalf("load: %d %v", len(got), err)
	}
	if got[0].Vector[0] != 1 || got[2].Vector[0] != 3 || got[0].Model != "m1" || got[0].Dims != 4 {
		t.Fatalf("roundtrip broken: %+v", got[0])
	}

	// model 过滤：m2 视角为空（换模型等效未建）
	if m2, _ := st.LoadDocumentVectors(ctx, "app", "user", doc.ID, "m2"); len(m2) != 0 {
		t.Fatalf("model filter broken: %d", len(m2))
	}

	// 替换为 2 条：旧行不残留
	st.ReplaceDocumentVectors(ctx, "app", "user", doc.ID, "m1", vecRows(doc.ID, 2, 4))
	if got, _ = st.LoadDocumentVectors(ctx, "app", "user", doc.ID, "m1"); len(got) != 2 {
		t.Fatalf("replace broken: %d", len(got))
	}
}

func TestLoadVectorHitsJoin(t *testing.T) {
	st, _ := openStore(t, "chunkvec-join.db")
	ctx := context.Background()
	doc, _ := st.UpsertDocument(ctx, "app", "user", "https://x.invalid/j.pdf", "JOIN书")
	chunks := idxTestChunks(doc.ID, 3)
	st.ReplaceDocumentIndex(ctx, "app", "user", doc.ID, chunks, &storage.DocumentIndexState{
		DocumentID: doc.ID, IndexStatus: storage.IndexStatusReady, ChunkCount: 3,
	})
	// 向量 chunk_id 对齐 chunks
	rows := vecRows(doc.ID, 3, 4)
	for i, c := range chunks {
		rows[i].ChunkID = c.ID
		rows[i].ChunkIndex = c.ChunkIndex
	}
	st.ReplaceDocumentVectors(ctx, "app", "user", doc.ID, "m1", rows)

	// 其他 user 的同名文档不可见（租户隔离）
	other, _ := st.UpsertDocument(ctx, "app", "user-b", "https://x.invalid/j.pdf", "他人")
	st.ReplaceDocumentVectors(ctx, "app", "user-b", other.ID, "m1", vecRows(other.ID, 2, 4))

	hits, vecs, err := st.LoadVectorHits(ctx, "app", "user", "m1", nil)
	if err != nil {
		t.Fatalf("load hits: %v", err)
	}
	if len(hits) != 3 || len(vecs) != 3 {
		t.Fatalf("join rows = %d/%d, want 3/3", len(hits), len(vecs))
	}
	if hits[0].Title != "JOIN书" || hits[0].Content == "" || vecs[0].ChunkID == "" {
		t.Fatalf("join fields missing: %+v", hits[0])
	}
	if len(vecs[0].Vector) != 4 {
		t.Fatalf("vector decode broken: %d", len(vecs[0].Vector))
	}
	// 范围过滤
	if hits, _, _ = st.LoadVectorHits(ctx, "app", "user", "m1", []string{doc.ID}); len(hits) != 3 {
		t.Fatalf("scoped = %d", len(hits))
	}
}

// TestChunksReplaceInvalidatesVectors chunks 重建 ⇒ 该文档全部向量（任何模型）
// 同事务作废——向量以 chunk_id 关联，chunk_id 全换后旧行必成孤儿，留着只会
// 膨胀存储且可能误配新块（P0 修复配套语义）。
func TestChunksReplaceInvalidatesVectors(t *testing.T) {
	st, _ := openStore(t, "chunkvec-invalidate.db")
	ctx := context.Background()
	doc, _ := st.UpsertDocument(ctx, "app", "user", "https://x.invalid/i.pdf", "作废验证")
	st.ReplaceDocumentIndex(ctx, "app", "user", doc.ID, idxTestChunks(doc.ID, 3), &storage.DocumentIndexState{
		DocumentID: doc.ID, IndexStatus: storage.IndexStatusReady, ChunkCount: 3,
	})
	st.ReplaceDocumentVectors(ctx, "app", "user", doc.ID, "m1", vecRows(doc.ID, 3, 4))
	st.ReplaceDocumentVectors(ctx, "app", "user", doc.ID, "m2", vecRows(doc.ID, 3, 4))

	// 重建 chunks：m1/m2 向量必须一并清空
	st.ReplaceDocumentIndex(ctx, "app", "user", doc.ID, idxTestChunks(doc.ID, 2), &storage.DocumentIndexState{
		DocumentID: doc.ID, IndexedContentHash: "h2", IndexStatus: storage.IndexStatusReady, ChunkCount: 2,
	})
	if got, _ := st.LoadDocumentVectors(ctx, "app", "user", doc.ID, "m1"); len(got) != 0 {
		t.Fatalf("m1 vectors must be invalidated by chunks replace, got %d", len(got))
	}
	if got, _ := st.LoadDocumentVectors(ctx, "app", "user", doc.ID, "m2"); len(got) != 0 {
		t.Fatalf("m2 vectors must be invalidated by chunks replace, got %d", len(got))
	}
}

// TestUpdateDocumentVectorState 向量域状态独立读写：只覆盖 vector_* 四列，
// 不触碰词法状态（P0 修复配套——向量失败/就绪不再与词法 ready 混用一个字段）。
func TestUpdateDocumentVectorState(t *testing.T) {
	st, _ := openStore(t, "chunkvec-state.db")
	ctx := context.Background()
	doc, _ := st.UpsertDocument(ctx, "app", "user", "https://x.invalid/s.pdf", "状态验证")
	st.ReplaceDocumentIndex(ctx, "app", "user", doc.ID, idxTestChunks(doc.ID, 2), &storage.DocumentIndexState{
		DocumentID: doc.ID, IndexedContentHash: "h1", IndexStatus: storage.IndexStatusReady,
		ChunkerVersion: 1, TokenizerVersion: "t1", ChunkCount: 2,
	})

	// 失败态：词法字段保持不变
	if err := st.UpdateDocumentVectorState(ctx, "app", "user", doc.ID, storage.DocumentVectorState{
		Model: "m1", Status: storage.VectorStatusFailed, Error: "sidecar down",
	}); err != nil {
		t.Fatalf("update failed-state: %v", err)
	}
	state, err := st.LoadDocumentIndexState(ctx, "app", "user", doc.ID)
	if err != nil || state == nil {
		t.Fatalf("load state: %v", err)
	}
	if state.VectorStatus != storage.VectorStatusFailed || state.VectorModel != "m1" || state.VectorError != "sidecar down" {
		t.Fatalf("vector state mismatch: %+v", state)
	}
	if state.IndexStatus != storage.IndexStatusReady || state.ChunkCount != 2 || state.IndexedContentHash != "h1" {
		t.Fatalf("lexical fields must be untouched: %+v", state)
	}

	// 就绪态
	if err := st.UpdateDocumentVectorState(ctx, "app", "user", doc.ID, storage.DocumentVectorState{
		Model: "m1", Status: storage.VectorStatusReady, Count: 2,
	}); err != nil {
		t.Fatalf("update ready-state: %v", err)
	}
	if state, _ = st.LoadDocumentIndexState(ctx, "app", "user", doc.ID); state.VectorStatus != storage.VectorStatusReady ||
		state.VectorCount != 2 || state.VectorError != "" {
		t.Fatalf("ready state mismatch: %+v", state)
	}

	// chunks=nil 的状态覆盖（失败标记路径）保留向量域：向量表数据未删除，
	// 若清空 vector_* 会造成「数据仍在、状态显示未建」，下次 BuildIndex 误判
	// 为需重建而重复 embed（词法构建失败本就少见，但仍需状态自洽）。
	st.ReplaceDocumentIndex(ctx, "app", "user", doc.ID, nil, &storage.DocumentIndexState{
		DocumentID: doc.ID, IndexStatus: storage.IndexStatusFailed, ErrorMessage: "boom",
	})
	if state, _ = st.LoadDocumentIndexState(ctx, "app", "user", doc.ID); state.VectorStatus != storage.VectorStatusReady ||
		state.VectorModel != "m1" || state.VectorCount != 2 {
		t.Fatalf("state-only overwrite must preserve vector domain: %+v", state)
	}
	if state, _ = st.LoadDocumentIndexState(ctx, "app", "user", doc.ID); state.IndexStatus != storage.IndexStatusFailed ||
		state.ErrorMessage != "boom" {
		t.Fatalf("state-only overwrite must still mark lexical failure: %+v", state)
	}
}
