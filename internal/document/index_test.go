package document_test

// BuildIndex 幂等双域判定测试（0010 P0 修复）：词法 ready 不再挡住向量补建。
// 用真实 sqlite store + 可控 fake embedder 走完整链路：
//  1. 首次构建（embed 失败）→ 词法 ready + 向量 failed
//  2. embed 恢复后重建 → 只补向量（chunk_id 不变，词法索引不重写）
//  3. 换模型 → 向量重建到新模型
//  4. 向量就绪后再触发 → 完全跳过（幂等）
//
// 外部测试包：import tool/search_document 成环的约束同 synonym_eval_test.go。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"

	"absorb-agent/internal/document"
	"absorb-agent/internal/retrieval"
	_ "absorb-agent/internal/store/sqlite"
	"absorb-agent/internal/store/storage"
)

// fakeEmbedder 可控向量化替身：failNext 置真时 EmbedDocuments 返回错误一次。
type fakeEmbedder struct {
	model    string
	failNext bool
	calls    int
}

func (f *fakeEmbedder) Model() string { return f.model }

func (f *fakeEmbedder) EmbedDocuments(_ context.Context, texts []string) ([][]float32, error) {
	f.calls++
	if f.failNext {
		f.failNext = false
		return nil, errors.New("sidecar unavailable")
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{float32(f.calls), float32(i + 1)}
	}
	return out, nil
}

func (f *fakeEmbedder) EmbedQuery(_ context.Context, _ string) ([]float32, error) {
	return []float32{1, 0}, nil
}

func idxContent(t *testing.T, st storage.Storage, ctx context.Context, url, text string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(text))
	doc, err := st.UpsertDocument(ctx, "app", "user", url, "幂等验证")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := st.SaveDocumentContent(ctx, "app", "user", &storage.DocumentContent{
		URL: url, ContentText: text, CharCount: len([]rune(text)),
		ContentHash: hex.EncodeToString(sum[:]), ContentType: "text/plain",
	}); err != nil {
		t.Fatalf("save content: %v", err)
	}
	return doc.ID
}

func chunkIDs(t *testing.T, st storage.Storage, ctx context.Context, docID string) []string {
	t.Helper()
	chunks, err := st.LoadDocumentChunks(ctx, "app", "user", docID)
	if err != nil {
		t.Fatalf("load chunks: %v", err)
	}
	ids := make([]string, len(chunks))
	for i, c := range chunks {
		ids[i] = c.ID
	}
	return ids
}

func TestBuildIndexVectorRetry(t *testing.T) {
	st, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()
	text := "这是用于幂等验证的正文内容。软件工程的实践会随着智能体的加入而改变。足够长以切出多个分块，幂等验证需要至少两个分块才能观察向量数量对齐。"
	docID := idxContent(t, st, ctx, "https://x.invalid/retry.pdf", text)

	emb := &fakeEmbedder{model: "m1", failNext: true}
	svc := document.NewService(st, nil, "app", "user").WithEmbedder(emb)

	// 第一次：embed 失败 → 词法 ready、向量 failed（不再被 ready 一刀切挡住）
	svc.BuildIndex(ctx, docID, mustContent(t, st, ctx, "https://x.invalid/retry.pdf"))
	state, err := st.LoadDocumentIndexState(ctx, "app", "user", docID)
	if err != nil || state == nil {
		t.Fatalf("load state: %v", err)
	}
	if state.IndexStatus != storage.IndexStatusReady || state.ChunkCount == 0 {
		t.Fatalf("lexical must be ready: %+v", state)
	}
	if state.VectorStatus != storage.VectorStatusFailed || state.VectorModel != "m1" {
		t.Fatalf("vector state must record failure: %+v", state)
	}
	ids1 := chunkIDs(t, st, ctx, docID)

	// 第二次（embed 恢复）：只补向量——chunk_id 不变证明词法索引未被重写
	svc.BuildIndex(ctx, docID, mustContent(t, st, ctx, "https://x.invalid/retry.pdf"))
	state, _ = st.LoadDocumentIndexState(ctx, "app", "user", docID)
	if state.VectorStatus != storage.VectorStatusReady ||
		state.VectorCount != state.ChunkCount || state.VectorError != "" {
		t.Fatalf("vector retry did not complete: %+v", state)
	}
	ids2 := chunkIDs(t, st, ctx, docID)
	if len(ids1) != len(ids2) {
		t.Fatalf("chunk count drifted: %d -> %d", len(ids1), len(ids2))
	}
	for i := range ids1 {
		if ids1[i] != ids2[i] {
			t.Fatalf("lexical index was rewritten on vector-only rebuild (chunk %d id changed)", i)
		}
	}

	// 向量就绪后再次触发：完全跳过（embed 调用数不再增长）
	callsBefore := emb.calls
	svc.BuildIndex(ctx, docID, mustContent(t, st, ctx, "https://x.invalid/retry.pdf"))
	if emb.calls != callsBefore {
		t.Fatalf("up-to-date index must skip embedding: calls %d -> %d", callsBefore, emb.calls)
	}

	// 换模型：向量域检测到 model 不匹配 → 重建到 m2（词法仍不重写）
	emb.model = "m2"
	svc.BuildIndex(ctx, docID, mustContent(t, st, ctx, "https://x.invalid/retry.pdf"))
	state, _ = st.LoadDocumentIndexState(ctx, "app", "user", docID)
	if state.VectorModel != "m2" || state.VectorStatus != storage.VectorStatusReady ||
		state.VectorCount != state.ChunkCount {
		t.Fatalf("model switch must re-embed: %+v", state)
	}
	if got, _ := st.LoadDocumentVectors(ctx, "app", "user", docID, "m2"); len(got) != state.ChunkCount {
		t.Fatalf("m2 vector rows = %d, want %d", len(got), state.ChunkCount)
	}
}

// newTestStore 打开临时文件库（每次独立，不依赖开发库副本）。
func newTestStore(t *testing.T) (storage.Storage, func()) {
	t.Helper()
	st, err := storage.New("sqlite", filepath.Join(t.TempDir(), "buildindex-test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return st, func() { _ = st.Close() }
}

func mustContent(t *testing.T, st storage.Storage, ctx context.Context, url string) *storage.DocumentContent {
	t.Helper()
	dc, err := st.LoadDocumentContent(ctx, "app", "user", url)
	if err != nil || dc == nil {
		t.Fatalf("load content: (%v, %v)", dc, err)
	}
	return dc
}

// 编译期确认接缝：fakeEmbedder 必须满足 Embedder 接口
var _ retrieval.Embedder = (*fakeEmbedder)(nil)
