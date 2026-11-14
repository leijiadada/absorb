package document

// Prepare 服务与 /api/page/prepare 端点测试：状态矩阵（ready/partial/失败态）、
// 落库语义（成功才落库、documentId=URL、归属隔离）、SSRF 拦截、HTTP 协议行为。

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"absorb-agent/internal/document/extract"
	"absorb-agent/internal/store/storage"
	retriever "absorb-agent/tool/pageretriever"

	// 注册 sqlite 后端，storage.New("sqlite", …) 可用
	_ "absorb-agent/internal/store/sqlite"
)

// fakeExtractor 可编程替身：返回预置结果或错误，并可断言"未被调用"。
type fakeExtractor struct {
	res    *extract.Result
	err    error
	called int
}

func (f *fakeExtractor) Extract(ctx context.Context, url string) (*extract.Result, error) {
	f.called++
	return f.res, f.err
}

func newTestStore(t *testing.T) storage.Storage {
	t.Helper()
	st, err := storage.New("sqlite", filepath.Join(t.TempDir(), "document-test.db"))
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

const testURL = "https://example.com/paper.pdf"

func TestPrepareReady(t *testing.T) {
	st := newTestStore(t)
	fx := &fakeExtractor{res: &extract.Result{
		Content: "第一段正文。\n第二段正文。", ContentType: "application/pdf", FinalURL: testURL,
	}}
	svc := NewService(st, fx, "app", "user")

	res, err := svc.Prepare(context.Background(), testURL, "研究论文")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if res.Status != extract.StatusReady || res.CharCount != len([]rune("第一段正文。\n第二段正文。")) {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.DocumentID != testURL || res.ContentType != "application/pdf" || res.Title != "研究论文" {
		t.Fatalf("metadata mismatch: %+v", res)
	}
	// 正文落库（完整正文，未注入截断）
	dc, err := st.LoadDocumentContent(context.Background(), "app", "user", testURL)
	if err != nil || dc == nil {
		t.Fatalf("load content: (%v, %v)", dc, err)
	}
	if dc.ContentText != "第一段正文。\n第二段正文。" || dc.ExtractionStatus != extract.StatusReady || dc.ContentHash == "" {
		t.Fatalf("stored content mismatch: %+v", dc)
	}
}

func TestPreparePartialLongText(t *testing.T) {
	st := newTestStore(t)
	long := strings.Repeat("字", retriever.PageTextMaxLen+100) // 超 30k rune
	fx := &fakeExtractor{res: &extract.Result{Content: long, ContentType: "application/pdf"}}
	svc := NewService(st, fx, "app", "user")

	res, err := svc.Prepare(context.Background(), testURL, "")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if res.Status != extract.StatusPartial {
		t.Fatalf("status = %s, want partial", res.Status)
	}
	// 全文仍完整落库（partial 只表达注入截断语义）
	dc, _ := st.LoadDocumentContent(context.Background(), "app", "user", testURL)
	if dc == nil || dc.CharCount != retriever.PageTextMaxLen+100 || dc.ContentText != long {
		t.Fatalf("full text must be stored, got char_count=%d", dc.CharCount)
	}

	// 获取侧静默截断（HTML 达上限）同样标 partial
	fx2 := &fakeExtractor{res: &extract.Result{Content: "短文本", ContentType: "text/html", Truncated: true}}
	svc2 := NewService(st, fx2, "app", "user")
	res2, err := svc2.Prepare(context.Background(), "https://example.com/page.html", "")
	if err != nil {
		t.Fatalf("Prepare html: %v", err)
	}
	if res2.Status != extract.StatusPartial {
		t.Fatalf("truncated html status = %s, want partial", res2.Status)
	}
}

func TestPrepareFailureStatesNotPersisted(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	cases := []struct {
		name string
		fx   *fakeExtractor
		want string
	}{
		{"需要登录", &fakeExtractor{err: &extract.Error{Status: extract.StatusNeedsAuth, Message: "401"}}, extract.StatusNeedsAuth},
		{"超大小上限", &fakeExtractor{err: &extract.Error{Status: extract.StatusTooLarge, Message: "exceeds"}}, extract.StatusTooLarge},
		{"加密", &fakeExtractor{err: &extract.Error{Status: extract.StatusEncrypted, Message: "encrypt"}}, extract.StatusEncrypted},
		{"扫描件", &fakeExtractor{res: &extract.Result{Content: "   \n  "}}, extract.StatusNoText},
		{"未知错误归网络错误", &fakeExtractor{err: errors.New("dial tcp: timeout")}, extract.StatusNetworkError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := NewService(st, tc.fx, "app", "user")
			res, err := svc.Prepare(ctx, testURL, "")
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			if res.Status != tc.want {
				t.Fatalf("status = %s, want %s", res.Status, tc.want)
			}
			// 失败状态不落库
			if dc, _ := st.LoadDocumentContent(ctx, "app", "user", testURL); dc != nil {
				t.Fatalf("failure state must not persist, got %+v", dc)
			}
		})
	}
}

func TestPrepareInvalidURLBlockedBeforeExtractor(t *testing.T) {
	st := newTestStore(t)
	fx := &fakeExtractor{res: &extract.Result{Content: "x"}}
	svc := NewService(st, fx, "app", "user")

	for _, u := range []string{
		"http://127.0.0.1:7788/secret",
		"http://169.254.169.254/latest/meta-data",
		"http://localhost/x",
		"file:///etc/passwd",
		"",
	} {
		res, err := svc.Prepare(context.Background(), u, "")
		if err != nil {
			t.Fatalf("Prepare(%q): %v", u, err)
		}
		if res.Status != extract.StatusInvalidURL {
			t.Fatalf("Prepare(%q) status = %s, want invalid_url", u, res.Status)
		}
	}
	if fx.called != 0 {
		t.Fatalf("extractor must not be called for blocked URLs, called=%d", fx.called)
	}
}

func TestPrepareReprepareOverwrites(t *testing.T) {
	st := newTestStore(t)
	fx := &fakeExtractor{res: &extract.Result{Content: "旧正文", ContentType: "application/pdf"}}
	svc := NewService(st, fx, "app", "user")
	if _, err := svc.Prepare(context.Background(), testURL, "标题1"); err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	fx.res = &extract.Result{Content: "新正文（重新读取）", ContentType: "application/pdf"}
	res, err := svc.Prepare(context.Background(), testURL, "标题2")
	if err != nil {
		t.Fatalf("re-prepare: %v", err)
	}
	if res.CharCount != len([]rune("新正文（重新读取）")) {
		t.Fatalf("char count = %d", res.CharCount)
	}
	dc, _ := st.LoadDocumentContent(context.Background(), "app", "user", testURL)
	if dc == nil || !strings.Contains(dc.ContentText, "重新读取") {
		t.Fatalf("content not refreshed: %+v", dc)
	}
	// 归属隔离：其他 user 查无
	if dc2, _ := st.LoadDocumentContent(context.Background(), "app", "user-b", testURL); dc2 != nil {
		t.Fatal("user isolation broken")
	}
}

func TestPagePrepareHTTP(t *testing.T) {
	st := newTestStore(t)
	fx := &fakeExtractor{res: &extract.Result{
		Content: "正文内容", ContentType: "application/pdf", FinalURL: testURL,
	}}
	svc := NewService(st, fx, "app", "user")

	mux := http.NewServeMux()
	NewAPI(svc).Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	do := func(method, body string) (int, string) {
		req, _ := http.NewRequest(method, ts.URL+"/api/page/prepare", strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, body, err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		var buf bytes.Buffer
		buf.ReadFrom(resp.Body)
		return resp.StatusCode, buf.String()
	}

	// 正常准备：200 + 状态与 documentId（=URL），响应不含正文
	code, body := do("POST", `{"url":"`+testURL+`","title":"论文"}`)
	if code != 200 {
		t.Fatalf("prepare status = %d, body=%s", code, body)
	}
	if !strings.Contains(body, `"status":"ready"`) || !strings.Contains(body, `"documentId":"`+testURL+`"`) {
		t.Fatalf("response missing fields: %s", body)
	}
	if strings.Contains(body, "正文内容") {
		t.Fatal("response must not contain content text")
	}

	// 内网 URL：200 + invalid_url（产品语义而非协议错误）
	code, body = do("POST", `{"url":"http://127.0.0.1:1/x"}`)
	if code != 200 || !strings.Contains(body, `"status":"invalid_url"`) {
		t.Fatalf("ssrf case: code=%d body=%s", code, body)
	}

	// 请求体坏 JSON：400
	if code, _ := do("POST", `{bad`); code != 400 {
		t.Fatalf("bad json status = %d, want 400", code)
	}

	// OPTIONS 预检：204
	req, _ := http.NewRequest("OPTIONS", ts.URL+"/api/page/prepare", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("OPTIONS: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("OPTIONS status = %d, want 204", resp.StatusCode)
	}
}
