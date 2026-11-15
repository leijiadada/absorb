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
