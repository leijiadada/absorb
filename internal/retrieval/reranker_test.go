package retrieval

// reranker_test.go —— RerankPoolCandidates（F4b 扩池精排）行为测试：
// 池构成（融合头 ∪ 向量种子、去重、融合序在前）、整池重排、池外融合尾
// 原序缀后、失败/未配置降级原融合榜。

import (
	"context"
	"errors"
	"testing"

	"absorb-agent/internal/store/storage"
)

type stubReranker struct {
	scores []float64
	err    error
}

func (s stubReranker) Rerank(ctx context.Context, query string, docs []string) ([]float64, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.scores, nil
}

func mkHit(idx int) storage.DocumentChunkHit {
	return storage.DocumentChunkHit{
		DocumentID: "d",
		ChunkIndex: idx,
		Content:    string(rune('A'+idx)) + " content",
	}
}

// ids 输出命中的 chunkIndex 序列，便于断言顺序。
func ids(hits []storage.DocumentChunkHit) []int {
	out := make([]int, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.ChunkIndex)
	}
	return out
}

func assertOrder(t *testing.T, got []int, want ...int) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("顺序长度不符: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("顺序不符: got %v want %v", got, want)
		}
	}
}

func TestRerankPoolCandidates(t *testing.T) {
	ctx := context.Background()
	// fused: F0..F5（融合序）；vec seeds: [3, 6, 7]（前 3 名，其中 3 与融合头重叠）
	fused := []storage.DocumentChunkHit{mkHit(0), mkHit(1), mkHit(2), mkHit(3), mkHit(4), mkHit(5)}
	seeds := []storage.DocumentChunkHit{mkHit(3), mkHit(6), mkHit(7)}
	// 分数按池内位置升序给分（越靠后分越高）→ 重排后池序完全倒置：
	// 池 = [F0,F1,F2,F3,S6,S7] → [S7,S6,F3,F2,F1,F0]
	asc := func(n int) []float64 {
		s := make([]float64, n)
		for i := range s {
			s[i] = float64(i)
		}
		return s
	}

	// 1) 池构成 + 整池重排 + 池外融合尾缀后
	out, applied := RerankPoolCandidates(ctx, fused, seeds, "q", stubReranker{scores: asc(6)}, 4, 3)
	if !applied {
		t.Fatal("applied=false")
	}
	assertOrder(t, ids(out), 7, 6, 3, 2, 1, 0, 4, 5)

	// 2) 精排失败降级原融合榜
	out, applied = RerankPoolCandidates(ctx, fused, seeds, "q", stubReranker{err: errors.New("boom")}, 4, 3)
	if applied {
		t.Fatal("失败不应 applied")
	}
	assertOrder(t, ids(out), 0, 1, 2, 3, 4, 5)

	// 3) 未配置精排器
	out, applied = RerankPoolCandidates(ctx, fused, seeds, "q", nil, 4, 3)
	if applied {
		t.Fatal("nil reranker 不应 applied")
	}
	assertOrder(t, ids(out), 0, 1, 2, 3, 4, 5)

	// 4) seedN=0 退化为 RerankCandidates（只重排融合头）
	out, applied = RerankPoolCandidates(ctx, fused, seeds, "q", stubReranker{scores: asc(4)}, 4, 0)
	if !applied {
		t.Fatal("applied=false")
	}
	assertOrder(t, ids(out), 3, 2, 1, 0, 4, 5)
}
