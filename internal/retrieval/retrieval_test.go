package retrieval

// RRF 融合与余弦召回测试（M3）。

import (
	"context"
	"testing"

	"absorb-agent/internal/store/storage"
)

func hit(doc string, idx int) storage.DocumentChunkHit {
	return storage.DocumentChunkHit{DocumentID: doc, ChunkIndex: idx, Title: "T", Content: "c"}
}

// TestFuseRRFBothRanks 同 chunk 双路命中分数叠加，排到最前。
func TestFuseRRFBothRanks(t *testing.T) {
	lexical := []storage.DocumentChunkHit{hit("d", 0), hit("d", 1)}
	vector := []storage.DocumentChunkHit{hit("d", 1), hit("d", 2)}
	fused := FuseRRF(lexical, vector, 10)
	if len(fused) != 3 {
		t.Fatalf("fused = %d, want 3", len(fused))
	}
	// d#1 双路命中（1/(60+1)+1/(60+1)）> 任何单路 top1（1/61）
	if fused[0].ChunkIndex != 1 {
		t.Fatalf("top = chunk %d, want both-route chunk 1", fused[0].ChunkIndex)
	}
}

// TestFuseRRFLexicalOnly 语义路为空 = 纯词法透传（保序）。
func TestFuseRRFLexicalOnly(t *testing.T) {
	lexical := []storage.DocumentChunkHit{hit("d", 5), hit("d", 3)}
	fused := FuseRRF(lexical, nil, 10)
	if len(fused) != 2 || fused[0].ChunkIndex != 5 || fused[1].ChunkIndex != 3 {
		t.Fatalf("lexical-only passthrough broken: %+v", fused)
	}
}

// TestFuseRRFLimit 总量裁剪。
func TestFuseRRFLimit(t *testing.T) {
	var lexical []storage.DocumentChunkHit
	for i := 0; i < 20; i++ {
		lexical = append(lexical, hit("d", i))
	}
	fused := FuseRRF(lexical, nil, 8)
	if len(fused) != 8 {
		t.Fatalf("limit broken: %d", len(fused))
	}
}

// TestTopKByCosine 余弦排序 + 维度不匹配行跳过。
func TestTopKByCosine(t *testing.T) {
	q := []float32{1, 0}
	rows := []storage.ChunkVectorRow{
		{ChunkIndex: 0, Vector: []float32{1, 0}},   // 相似度 1
		{ChunkIndex: 1, Vector: []float32{0, 1}},   // 相似度 0
		{ChunkIndex: 2, Vector: []float32{0.7, 0.7}}, // ≈0.707
		{ChunkIndex: 3, Vector: []float32{9, 9, 9}},  // dims 不匹配，跳过
		{ChunkIndex: 4, Vector: []float32{-1, 0}},  // 相似度 -1
	}
	hits := make([]storage.DocumentChunkHit, len(rows))
	for i := range rows {
		hits[i] = hit("d", rows[i].ChunkIndex)
	}
	top := TopKByCosine(hits, rows, q, 3)
	if len(top) != 3 {
		t.Fatalf("top = %d, want 3（dims 不匹配行跳过）", len(top))
	}
	if top[0].ChunkIndex != 0 || top[1].ChunkIndex != 2 || top[2].ChunkIndex != 1 {
		t.Fatalf("cosine order broken: %d,%d,%d", top[0].ChunkIndex, top[1].ChunkIndex, top[2].ChunkIndex)
	}
}

// TestOpenAICompatEmbedder httptest 全链路：请求体/响应解析/批切分。
func TestOpenAICompatEmbedder(t *testing.T) {
	var gotBatches [][]string
	srv := newEmbedTestServer(t, &gotBatches)
	defer srv.Close()
	e := NewOpenAICompatEmbedder(srv.URL, "sk-test", "test-model", 2, func() string { return "" })
	vecs, err := e.EmbedDocuments(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(vecs) != 3 || len(vecs[0]) != 4 {
		t.Fatalf("vecs = %d", len(vecs))
	}
	if len(gotBatches) != 2 || len(gotBatches[0]) != 2 || len(gotBatches[1]) != 1 {
		t.Fatalf("batch split broken: %v", gotBatches)
	}
}
