package knowledge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// newTestHTTPWithDraft 起一个注入了 DraftFunc 的知识端点测试服务。
func newTestHTTPWithDraft(t *testing.T, fn DraftFunc) *httptest.Server {
	t.Helper()
	svc := NewService(newTestStore(t))
	mux := http.NewServeMux()
	NewHTTP(svc, "app", "user").SetDraft(fn).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

const draftBody = `{"question":"为什么 RAG 的效果不只取决于生成模型？","answer":"RAG 的质量还取决于检索、排序与上下文构建。","quote":"检索阶段找到正确资料是前提。"}`

func TestDraftOK(t *testing.T) {
	var got DraftPrompt
	srv := newTestHTTPWithDraft(t, func(ctx context.Context, in DraftPrompt) (DraftResult, error) {
		got = in
		return DraftResult{Title: "RAG 效果先受检索质量约束", Summary: "检索错误则生成不可靠。"}, nil
	})
	resp := postBody(t, srv.URL+"/api/knowledge/draft", draftBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	m := decode(t, resp)
	if m["ok"] != true || m["title"] != "RAG 效果先受检索质量约束" || m["summary"] != "检索错误则生成不可靠。" {
		t.Fatalf("unexpected body: %v", m)
	}
	if got.Question == "" || got.Answer == "" || got.Quote == "" {
		t.Fatalf("prompt fields lost: %+v", got)
	}
}

func TestDraftNilFnFallsBack(t *testing.T) {
	srv := newTestHTTPWithDraft(t, nil) // 未注入（如 CLI 形态漏配）→ 端点仍在，直接降级
	resp := postBody(t, srv.URL+"/api/knowledge/draft", draftBody)
	defer resp.Body.Close()
	m := decode(t, resp)
	if m["ok"] != false {
		t.Fatalf("ok = %v, want false", m["ok"])
	}
	if m["title"] == "" || m["summary"] != "" {
		t.Fatalf("fallback should carry title only: %v", m)
	}
	if !strings.Contains(m["title"].(string), "RAG") {
		t.Fatalf("fallback title should derive from question: %v", m["title"])
	}
}

func TestDraftErrorFallsBack(t *testing.T) {
	srv := newTestHTTPWithDraft(t, func(ctx context.Context, in DraftPrompt) (DraftResult, error) {
		return DraftResult{}, errors.New("llm timeout")
	})
	resp := postBody(t, srv.URL+"/api/knowledge/draft", draftBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("degraded draft must still be 200, got %d", resp.StatusCode)
	}
	m := decode(t, resp)
	if m["ok"] != false || m["summary"] != "" {
		t.Fatalf("unexpected degraded body: %v", m)
	}
}

func TestDraftEmptyTitleFallsBack(t *testing.T) {
	srv := newTestHTTPWithDraft(t, func(ctx context.Context, in DraftPrompt) (DraftResult, error) {
		return DraftResult{Title: "   "}, nil // 模型认为材料不足 → 降级
	})
	resp := postBody(t, srv.URL+"/api/knowledge/draft", draftBody)
	defer resp.Body.Close()
	m := decode(t, resp)
	if m["ok"] != false {
		t.Fatalf("empty title should degrade, got %v", m)
	}
}

func TestDraftBadBody400(t *testing.T) {
	srv := newTestHTTPWithDraft(t, func(ctx context.Context, in DraftPrompt) (DraftResult, error) {
		t.Fatal("draftFn must not be called on bad body")
		return DraftResult{}, nil
	})
	resp := postBody(t, srv.URL+"/api/knowledge/draft", "{not json")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestDraftInputClamped(t *testing.T) {
	var got DraftPrompt
	srv := newTestHTTPWithDraft(t, func(ctx context.Context, in DraftPrompt) (DraftResult, error) {
		got = in
		return DraftResult{Title: "t"}, nil
	})
	long := strings.Repeat("长", 10000)
	resp := postBody(t, srv.URL+"/api/knowledge/draft",
		`{"question":"`+long+`","answer":"`+long+`","quote":"`+long+`"}`)
	defer resp.Body.Close()
	decode(t, resp)
	if utf8.RuneCountInString(got.Question) > draftMaxQuestion {
		t.Fatalf("question not clamped: %d", utf8.RuneCountInString(got.Question))
	}
	if utf8.RuneCountInString(got.Answer) > draftMaxAnswer {
		t.Fatalf("answer not clamped: %d", utf8.RuneCountInString(got.Answer))
	}
	if utf8.RuneCountInString(got.Quote) > draftMaxQuote {
		t.Fatalf("quote not clamped: %d", utf8.RuneCountInString(got.Quote))
	}
}

func TestFallbackTitleTruncates(t *testing.T) {
	q := strings.Repeat("问", 100)
	got := fallbackTitle(q)
	if utf8.RuneCountInString(got) != draftMaxTitle {
		t.Fatalf("len = %d, want %d", utf8.RuneCountInString(got), draftMaxTitle)
	}
	if fallbackTitle("   ") != "未命名知识" {
		t.Fatalf("empty question should map to 未命名知识")
	}
}
