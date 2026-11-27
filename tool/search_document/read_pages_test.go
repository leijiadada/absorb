package searchdoc

// read_pages_test.go 页级阅读模式：基本读取/锚定校验/无分页降级/互斥提示/
// 预算截断/越界提示/检索结果页码标注。

import (
	"context"
	"strings"
	"testing"

	"absorb-agent/internal/store/storage"
)

// fakeContentReader 固定正文的内容读取替身。
type fakeContentReader struct {
	text string
}

func (f *fakeContentReader) LoadDocumentContentByID(ctx context.Context, app, user, docID string) (*storage.DocumentContent, error) {
	return &storage.DocumentContent{ContentText: f.text}, nil
}

// anchoredState 单文档锚定 + 3 页正文（甲/乙/丙各一页）。
func anchoredState() context.Context {
	text := "甲页内容\f乙页内容\f丙页内容"
	return WithDocumentQueryState(context.Background(), &fakeSearcher{},
		nil, nil, &fakeContentReader{text: text}, "app", "user", []string{"d1"})
}

func TestReadPagesBasic(t *testing.T) {
	out := callTool(t, anchoredState(), searchDocumentInput{StartPage: 2, PageCount: 2})
	if !out.Success {
		t.Fatalf("expect success: %+v", out)
	}
	for _, want := range []string{"—— 第 2 页 ——", "乙页内容", "—— 第 3 页 ——", "丙页内容"} {
		if !strings.Contains(out.Content, want) {
			t.Fatalf("read missing %q:\n%s", want, out.Content)
		}
	}
}

func TestReadPagesUnanchored(t *testing.T) {
	ctx := WithDocumentQueryState(context.Background(), &fakeSearcher{},
		nil, nil, &fakeContentReader{text: "甲\f乙"}, "app", "user", nil)
	out := callTool(t, ctx, searchDocumentInput{StartPage: 1})
	if out.Success || !strings.Contains(out.Message, "锚定") {
		t.Fatalf("unanchored mismatch: %+v", out)
	}
}

func TestReadPagesNoPagination(t *testing.T) {
	ctx := WithDocumentQueryState(context.Background(), &fakeSearcher{},
		nil, nil, &fakeContentReader{text: "无分页的连续正文"}, "app", "user", []string{"d1"})
	out := callTool(t, ctx, searchDocumentInput{StartPage: 1})
	if out.Success || !strings.Contains(out.Message, "无分页信息") {
		t.Fatalf("no pagination mismatch: %+v", out)
	}
}

func TestReadPagesEndOfDoc(t *testing.T) {
	out := callTool(t, anchoredState(), searchDocumentInput{StartPage: 5})
	if !out.Success || !strings.Contains(out.Content, "已到文档末尾") {
		t.Fatalf("end of doc mismatch: %+v", out)
	}
}

func TestReadPagesBudgetTruncation(t *testing.T) {
	long := strings.Repeat("长", 6500)
	ctx := WithDocumentQueryState(context.Background(), &fakeSearcher{},
		nil, nil, &fakeContentReader{text: long + "\f第二页"}, "app", "user", []string{"d1"})
	out := callTool(t, ctx, searchDocumentInput{StartPage: 1, PageCount: 2})
	if !out.Success || !strings.Contains(out.Content, "本页已截断") {
		t.Fatalf("budget truncation missing: %+v", out)
	}
}

// 互斥规则：query 与 start_page 同填 → 按检索处理并附用法提示
func TestSearchMutualFillHint(t *testing.T) {
	fs := &fakeSearcher{hits: []storage.DocumentChunkHit{
		{DocumentID: "d1", Title: "手册", Content: "命中内容。", Score: -1},
	}}
	ctx := WithDocumentQueryState(context.Background(), fs,
		nil, nil, &fakeContentReader{text: "正文\f正文"}, "app", "user", []string{"d1"})
	out := callTool(t, ctx, searchDocumentInput{Query: "关键词", StartPage: 2})
	if !out.Success || !strings.Contains(out.Content, "已按检索处理") {
		t.Fatalf("mutual fill hint missing: %+v", out)
	}
}

// 检索结果页码标注：hit 偏移落在第 2 页 → 渲染含（p2）
func TestSearchPageAnnotation(t *testing.T) {
	fs := &fakeSearcher{hits: []storage.DocumentChunkHit{
		{DocumentID: "d1", Title: "手册", ChunkIndex: 1, SectionPath: "第二章",
			Content: "乙页命中。", StartOffset: 3, EndOffset: 9, Score: -1},
	}}
	ctx := WithDocumentQueryState(context.Background(), fs,
		nil, nil, &fakeContentReader{text: "甲页\f乙页\f丙页"}, "app", "user", []string{"d1"})
	out := callTool(t, ctx, searchDocumentInput{Query: "乙页"})
	if !out.Success || !strings.Contains(out.Content, "第二章（p2）") {
		t.Fatalf("page annotation missing: %+v", out)
	}
}

// 页号二分：pageOf 的边界偏移（页首/页中/末页）映射正确
func TestDocTextIndexPageOf(t *testing.T) {
	text := "甲\f乙\f丙\f丁"
	idx := &docTextIndex{text: []rune(text), pages: []int{0, 2, 4, 6}, paginated: true}
	cases := []struct {
		offset int
		want   int
	}{
		{0, 1}, {1, 1}, {2, 2}, {3, 2}, {6, 4}, {7, 4},
	}
	for _, c := range cases {
		if got := idx.pageOf(c.offset); got != c.want {
			t.Fatalf("pageOf(%d) = %d, want %d", c.offset, got, c.want)
		}
	}
	if _, ok := idx.pageText(5); ok {
		t.Fatalf("pageText 越界应返回 false")
	}
}
