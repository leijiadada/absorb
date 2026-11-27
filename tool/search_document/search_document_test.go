package searchdoc

// search_document 工具测试：无状态/空 query/零命中信号/命中渲染/错误降级/范围传递。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"absorb-agent/internal/store/storage"
)

type fakeSearcher struct {
	hits []storage.DocumentChunkHit
	err  error
	got  struct {
		app, user, query string
		docs             []string
		limit            int
	}
}

func (f *fakeSearcher) SearchDocumentChunks(ctx context.Context, app, user, query string, documentIDs []string, limit int) ([]storage.DocumentChunkHit, error) {
	f.got.app, f.got.user, f.got.query, f.got.docs, f.got.limit = app, user, query, documentIDs, limit
	return f.hits, f.err
}

// callTool 经真实 FunctionTool 全链路（JSON 解析 → fn → 类型化输出）。
func callTool(t *testing.T, ctx context.Context, in searchDocumentInput) searchDocumentOutput {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	res, err := NewSearchDocumentTool().Call(ctx, raw)
	if err != nil {
		t.Fatalf("tool call: %v", err)
	}
	out, ok := res.(searchDocumentOutput)
	if !ok {
		t.Fatalf("result type = %T", res)
	}
	return out
}

// 无状态：ctx 未注入 → 优雅失败（不 panic、给模型可读 message）
func TestSearchDocumentNoState(t *testing.T) {
	out := callTool(t, context.Background(), searchDocumentInput{Query: "检索"})
	if out.Success || !strings.Contains(out.Message, "不可用") {
		t.Fatalf("no state mismatch: %+v", out)
	}
}

// 空 query：参数校验
func TestSearchDocumentEmptyQuery(t *testing.T) {
	ctx := WithDocumentSearchState(context.Background(), &fakeSearcher{}, "app", "user", []string{"d1"})
	out := callTool(t, ctx, searchDocumentInput{Query: "   "})
	if out.Success || !strings.Contains(out.Message, "query") {
		t.Fatalf("empty query mismatch: %+v", out)
	}
}

// 零命中：返回信号而非错误，引导模型换词；scope 文档范围透传检索层
func TestSearchDocumentZeroHitSignal(t *testing.T) {
	fs := &fakeSearcher{}
	ctx := WithDocumentSearchState(context.Background(), fs, "app", "user", []string{"d1"})
	out := callTool(t, ctx, searchDocumentInput{Query: "不存在的词"})
	if !out.Success || !strings.Contains(out.Content, "零命中") {
		t.Fatalf("zero-hit signal missing: %+v", out)
	}
	if fs.got.app != "app" || fs.got.user != "user" || len(fs.got.docs) != 1 || fs.got.docs[0] != "d1" {
		t.Fatalf("scope not passed: %+v", fs.got)
	}
	if fs.got.limit != RawRecallLimit {
		t.Fatalf("recall limit = %d, want %d（宽召回后由融合层裁剪）", fs.got.limit, RawRecallLimit)
	}
}

// 命中渲染（M2 分组版）：按资料聚合、组内列片段、标题计数
func TestSearchDocumentRendersHits(t *testing.T) {
	fs := &fakeSearcher{hits: []storage.DocumentChunkHit{
		{DocumentID: "d1", Title: "测试手册", ChunkIndex: 2, Content: "这是命中的一段原文，讨论软件工程。", StartOffset: 10, EndOffset: 26, Score: -1.5},
		{DocumentID: "d2", Title: "另一份资料", ChunkIndex: 0, Content: "另一段证据。", Score: -0.8},
	}}
	ctx := WithDocumentSearchState(context.Background(), fs, "app", "user", nil)
	out := callTool(t, ctx, searchDocumentInput{Query: "软件工程", TopK: 2})
	if !out.Success {
		t.Fatalf("expect success: %+v", out)
	}
	for _, want := range []string{"【资料 1】测试手册", "【资料 2】另一份资料", "命中的一段原文", "另一段证据"} {
		if !strings.Contains(out.Content, want) {
			t.Fatalf("render missing %q:\n%s", want, out.Content)
		}
	}
}

// 检索层错误：降级为 message（工具不向 agent 抛错，模型可继续作答）
func TestSearchDocumentSearcherError(t *testing.T) {
	ctx := WithDocumentSearchState(context.Background(), &fakeSearcher{err: errors.New("boom")}, "app", "user", nil)
	out := callTool(t, ctx, searchDocumentInput{Query: "x"})
	if out.Success || !strings.Contains(out.Message, "检索失败") {
		t.Fatalf("error degrade mismatch: %+v", out)
	}
}
