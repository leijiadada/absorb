package chat

// scope 身份统一测试：同一文章经不同渠道链接（utm_*/fragment/尾斜杠/query
// 顺序差异）访问时，事件 FilterKey 必须落在同一归一键上（与页面缓存/摘要 key 同键）。
// 本测试用两个归一同键的 URL 变体先后提问，断言归一键下能索引到两次请求的事件。
import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/session"

	"absorb-agent/internal/kind"
	"absorb-agent/internal/store"
)

// askWithPage 发起一次带 page.url 的纯文本 /api/ask 请求（追问形态），返回 SSE 全文。
func askWithPage(t *testing.T, h http.HandlerFunc, pageURL, sessionID string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"question":  "这段在讲什么？",
		"page":      map[string]any{"url": pageURL, "title": "t"},
		"sessionId": sessionID,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/ask", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec.Body.String()
}

func TestAskHandler_ScopeUnifiedAcrossURLVariants(t *testing.T) {
	h, _, svc := setup(t, "openai", true, false, defaultOKStream)

	// 两个变体归一后同键 https://b.com/post/1（大写 host/fragment/尾斜杠/utm 差异全部抹平）
	normalized := "https://b.com/post/1"
	variants := []string{
		"https://B.com/post/1?utm_source=twitter#top",
		"https://b.com/post/1/?utm_source=wechat",
	}

	var sessionID string
	for i, u := range variants {
		out := askWithPage(t, h, u, sessionID)
		if strings.Contains(out, `"type":"error"`) {
			t.Fatalf("变体 %d 请求不应报错，SSE: %s", i, out)
		}
		if sessionID == "" {
			m := strings.Index(out, `"sessionId":"`)
			if m < 0 {
				t.Fatalf("SSE 未含 sessionId: %s", out)
			}
			rest := out[m+len(`"sessionId":"`):]
			sessionID = rest[:strings.Index(rest, `"`)]
		}
	}

	// 归一同键下应能索引到两次请求的事件（落库在 run 收尾异步完成，轮询等待）。
	// 同键 = ScopeID(归一网址)（定长哈希）；同时校验标识→网址映射已登记。
	key := session.Key{AppName: AppName, UserID: "absorb-user", SessionID: sessionID}
	scopeID := store.ScopeID(normalized)
	deadline := time.Now().Add(2 * time.Second)
	for {
		entries, err := svc.ListEventIndex(context.Background(), key, scopeID)
		if err != nil {
			t.Fatalf("ListEventIndex: %v", err)
		}
		if len(entries) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("标识 %q（归一网址 %q）下仅索引到 %d 条事件",
				scopeID, normalized, len(entries))
		}
		time.Sleep(50 * time.Millisecond)
	}
	urls, err := svc.ListScopeURLs(context.Background(), AppName, "absorb-user", sessionID)
	if err != nil {
		t.Fatalf("ListScopeURLs: %v", err)
	}
	if got := urls[scopeID]; got != normalized {
		t.Fatalf("标识→网址映射缺失或错配：urls[%q]=%q, want %q（全部映射 %v）", scopeID, got, normalized, urls)
	}
}

// TestAskHandler_GeneralScopeMarking 无页面、无划词的新会话提问（如浏览器新标签页）：
// 事件统一落 kind.ScopeGeneral（不留空标），scope_urls 映射登记可读占位。
func TestAskHandler_GeneralScopeMarking(t *testing.T) {
	h, _, svc := setup(t, "openai", true, false, defaultOKStream)

	body, _ := json.Marshal(map[string]any{"question": "什么是 RAG？"})
	req := httptest.NewRequest(http.MethodPost, "/api/ask", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, req)
	out := rec.Body.String()
	if strings.Contains(out, `"type":"error"`) {
		t.Fatalf("请求不应报错，SSE: %s", out)
	}
	m := strings.Index(out, `"sessionId":"`)
	if m < 0 {
		t.Fatalf("SSE 未含 sessionId: %s", out)
	}
	rest := out[m+len(`"sessionId":"`):]
	sessionID := rest[:strings.Index(rest, `"`)]

	// general 标识下应索引到本轮事件（user + assistant ≥2 条；落库在 run 收尾异步完成，
	// 轮询等待；若留空标则此处索引为 0）。
	key := session.Key{AppName: AppName, UserID: "absorb-user", SessionID: sessionID}
	deadline := time.Now().Add(2 * time.Second)
	for {
		entries, err := svc.ListEventIndex(context.Background(), key, kind.ScopeGeneral)
		if err != nil {
			t.Fatalf("ListEventIndex: %v", err)
		}
		if len(entries) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("general 标识下仅索引到 %d 条事件", len(entries))
		}
		time.Sleep(50 * time.Millisecond)
	}

	// 映射登记可读占位（日志/调试可溯源）
	urls, err := svc.ListScopeURLs(context.Background(), AppName, "absorb-user", sessionID)
	if err != nil {
		t.Fatalf("ListScopeURLs: %v", err)
	}
	if got := urls[kind.ScopeGeneral]; got != kind.ScopeGeneralDesc {
		t.Fatalf("general 映射缺失或错配：urls[%q]=%q, want %q", kind.ScopeGeneral, got, kind.ScopeGeneralDesc)
	}
}
