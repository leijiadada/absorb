package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"absorb-agent/internal/store"
	"absorb-agent/internal/store/storage"
	retriever "absorb-agent/tool/pageretriever"
)

// askWithContext 发起一次带 context（划词 + pageMd）的 /api/ask 请求，返回 SSE 全文。
func askWithContext(t *testing.T, h http.HandlerFunc, sessionID, pageURL, pageMd string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"question":  "这段在讲什么？",
		"sessionId": sessionID,
		"context": map[string]any{
			"text":   "术语",
			"url":    pageURL,
			"pageMd": pageMd,
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/ask", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec.Body.String()
}

// loadPageCache 读某 scope 的页面缓存（handler 的 SavePageText 在 SSE 首字节前完成，
// 请求同步返回后即可断言）。
func loadPageCache(t *testing.T, svc *store.SessionService, sessionID, pageURL string) string {
	t.Helper()
	got, err := svc.LoadPageText(context.Background(), AppName, UserID, sessionID, store.ScopeID(pageURL))
	if err != nil {
		t.Fatalf("LoadPageText: %v", err)
	}
	return got
}

// pageMd 直读：转换已在扩展端完成，缓存落 pageMd 原文。
func TestPageCacheWritesPageMd(t *testing.T) {
	h, _, svc := setup(t, "openai", true, false, defaultOKStream)
	const sid = "s-page-md"
	const url = "https://md-test.com/a"
	out := askWithContext(t, h, sid, url, "MD内容甲")
	if strings.Contains(out, `"type":"error"`) {
		t.Fatalf("请求不应报错，SSE: %s", out)
	}
	if got := loadPageCache(t, svc, sid, url); got != "MD内容甲" {
		t.Fatalf("pageMd 应直读入缓存，got %q", got)
	}
}

// 无划词提问 + 仅页面内容（context.text 为空）：写缓存 + prompt 注入 <page_content>，
// 不再注入 <page_url>/fetch_page 提示（页面内容已直接在场，省 LLM 工具轮次）。
func TestNoQuotePageMdInjectsPageContent(t *testing.T) {
	h, cap2, svc := setup(t, "openai", true, false, defaultOKStream)
	const sid = "s-page-only"
	const url = "https://spa-internal.example.com/setting/logs"
	body, _ := json.Marshal(map[string]any{
		"question":  "这个页面在讲什么？",
		"sessionId": sid,
		"page":      map[string]any{"url": url, "title": "日志管理"},
		"context": map[string]any{
			"text":   "",
			"url":    url,
			"pageMd": "操作日志页面内容：记录用户登录与查询行为",
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/ask", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, req)
	if strings.Contains(rec.Body.String(), `"type":"error"`) {
		t.Fatalf("请求不应报错，SSE: %s", rec.Body.String())
	}
	if got := loadPageCache(t, svc, sid, url); !strings.Contains(got, "操作日志页面内容") {
		t.Fatalf("pageMd 应直读入缓存，got %q", got)
	}
	// prompt 注入 <page_content>，不再注入 <page_url>。
	// 断言必须解析 user message 原文：请求体 JSON 会把 < 转义为 \u003c，
	// 且 <page_url> 字面量也出现在 recall_history 工具描述里，全文 Contains 会误报。
	reqBody := cap2.get()
	var sent struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(reqBody), &sent); err != nil {
		t.Fatalf("请求体解析失败: %v", err)
	}
	var userMsg string
	for _, m := range sent.Messages {
		if m.Role == "user" {
			if s, ok := m.Content.(string); ok {
				userMsg = s
			}
		}
	}
	if !strings.Contains(userMsg, "<page_content>") || !strings.Contains(userMsg, "操作日志页面内容") {
		t.Fatalf("prompt 应注入 <page_content>，user message: %q", userMsg)
	}
	if strings.Contains(userMsg, "<page_url>") {
		t.Fatalf("页面内容已注入，不应再注入 <page_url>，user message: %q", userMsg)
	}
}

// 文档注入（PDF 解读）：page.documentId 命中已提取正文 → prompt 注入
// <page_content>（正文可达模型），不再注入 <page_url>/fetch_page 提示；
// 正文同时入会话页面缓存（追问复用）。
// 回归守卫：PR2 初版只写缓存与 expand_context 工具态、未回写 askCtx.PageMd，
// prompt 里没有 <page_content>，模型看不到正文、反而被"按需 fetch_page"
// 提示误导（实测回复"把目录贴给我"）。
func TestDocumentIdInjectsPageContent(t *testing.T) {
	h, cap2, svc := setup(t, "openai", true, false, defaultOKStream)
	const sid = "s-doc-inject"
	const url = "https://paper.example.com/agenticse.pdf"
	dc := &storage.DocumentContent{
		URL:              url,
		ContentText:      "智能体工程全书正文内容：规划、工具与记忆",
		CharCount:        18,
		ContentType:      "application/pdf",
		ExtractionStatus: "ready",
		FetchedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}
	if err := svc.Storage().SaveDocumentContent(context.Background(), AppName, UserID, dc); err != nil {
		t.Fatalf("SaveDocumentContent: %v", err)
	}
	body, _ := json.Marshal(map[string]any{
		"question":  "这份文档讲什么？",
		"sessionId": sid,
		"page":      map[string]any{"url": url, "title": "论文", "documentId": url},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/ask", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, req)
	if strings.Contains(rec.Body.String(), `"type":"error"`) {
		t.Fatalf("请求不应报错，SSE: %s", rec.Body.String())
	}
	// 正文入会话页面缓存（无 documentId 的追问轮经 LoadPageText 复用）
	if got := loadPageCache(t, svc, sid, url); !strings.Contains(got, "智能体工程全书") {
		t.Fatalf("文档正文应写入页面缓存，got %q", got)
	}
	reqBody := cap2.get()
	var sent struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(reqBody), &sent); err != nil {
		t.Fatalf("请求体解析失败: %v", err)
	}
	var userMsg string
	for _, m := range sent.Messages {
		if m.Role == "user" {
			if s, ok := m.Content.(string); ok {
				userMsg = s
			}
		}
	}
	if !strings.Contains(userMsg, "<page_content>") || !strings.Contains(userMsg, "智能体工程全书") {
		t.Fatalf("prompt 应注入 <page_content> 且正文可达模型，user message: %q", userMsg)
	}
	if strings.Contains(userMsg, "<page_url>") {
		t.Fatalf("文档正文已注入，不应再注入 <page_url>，user message: %q", userMsg)
	}
}

// 文档划词（PDF 解读联调回归）：划词来源是文档页时，<page_content> 仍须注入
// （划词只是文档一小段，flash 模型不会主动调 expand_context）且划词材料不丢、
// scope 落文档 URL。查看器内划词的 URL 纠正在扩展端 quote-collector 源头完成，
// 服务端收到的 context.url 已是真实文档地址。
func TestSelectionWithDocumentIdInjectsPageContent(t *testing.T) {
	h, cap2, svc := setup(t, "openai", true, false, defaultOKStream)
	const sid = "s-doc-selection"
	const url = "https://paper.example.com/agenticse.pdf"
	dc := &storage.DocumentContent{
		URL:              url,
		ContentText:      "智能体工程全书正文内容：规划、工具与记忆",
		CharCount:        20,
		ContentType:      "application/pdf",
		ExtractionStatus: "ready",
		FetchedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}
	if err := svc.Storage().SaveDocumentContent(context.Background(), AppName, UserID, dc); err != nil {
		t.Fatalf("SaveDocumentContent: %v", err)
	}
	body, _ := json.Marshal(map[string]any{
		"question":  "这段讲的是什么？",
		"sessionId": sid,
		"context": map[string]any{
			"text": "AgenticSE Book",
			"url":  url, // 扩展端已纠正为真实文档地址
		},
		"page": map[string]any{"url": url, "title": "论文", "documentId": url},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/ask", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, req)
	if strings.Contains(rec.Body.String(), `"type":"error"`) {
		t.Fatalf("请求不应报错，SSE: %s", rec.Body.String())
	}
	// scope 归组落到真实文档 URL，而不是查看器扩展页地址
	if got, err := svc.RecentPageURL(context.Background(), AppName, UserID, sid); err != nil || got != url {
		t.Fatalf("scope url 应为文档地址，got %q err=%v", got, err)
	}
	reqBody := cap2.get()
	var sent struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(reqBody), &sent); err != nil {
		t.Fatalf("请求体解析失败: %v", err)
	}
	var userMsg string
	for _, m := range sent.Messages {
		if m.Role == "user" {
			if s, ok := m.Content.(string); ok {
				userMsg = s
			}
		}
	}
	if !strings.Contains(userMsg, "<page_content>") || !strings.Contains(userMsg, "智能体工程全书") {
		t.Fatalf("文档划词提问也应注入 <page_content>，user message: %q", userMsg)
	}
	if !strings.Contains(userMsg, "<reference_material>") || !strings.Contains(userMsg, "AgenticSE Book") {
		t.Fatalf("划词材料不应丢失，user message: %q", userMsg)
	}
}

// pageMd 为空（转换失败等）→ 不写缓存，走 LoadPageText 降级（宁缺勿错）。
func TestPageCacheEmptyMdFallsToLoadPath(t *testing.T) {
	h, _, svc := setup(t, "openai", true, false, defaultOKStream)
	const sid = "s-page-empty"
	const url = "https://md-test.com/b"
	out := askWithContext(t, h, sid, url, "   ")
	if strings.Contains(out, `"type":"error"`) {
		t.Fatalf("请求不应报错，SSE: %s", out)
	}
	if got := loadPageCache(t, svc, sid, url); got != "" {
		t.Fatalf("pageMd 为空时不应写缓存，got %q", got)
	}
}

// 截断守卫：pageMd 直读路径没有转换器内置截断，须经 TruncatePageText 同规格截断。
func TestPageCacheTruncatesOversizePageMd(t *testing.T) {
	h, _, svc := setup(t, "openai", true, false, defaultOKStream)
	const sid = "s-page-trunc"
	const url = "https://md-test.com/c"
	out := askWithContext(t, h, sid, url, strings.Repeat("字", 31000))
	if strings.Contains(out, `"type":"error"`) {
		t.Fatalf("请求不应报错，SSE: %s", out)
	}
	got := loadPageCache(t, svc, sid, url)
	// pageTextMaxLen(30000) + 尾标"\n…（已截断）"(7 runes)
	if n := len([]rune(got)); n != 30007 {
		t.Fatalf("pageMd 应截断为 30007 runes，实际 %d", n)
	}
	if !strings.HasSuffix(got, "…（已截断）") {
		t.Fatalf("截断尾标缺失，尾部: %q", []rune(got)[30000:])
	}
}

// TestTruncatePageText 纯函数守卫：短文本原样、超限截断（与 pageconvert 产出口径一致）。
func TestTruncatePageText(t *testing.T) {
	if got := retriever.TruncatePageText("短文本"); got != "短文本" {
		t.Fatalf("短文本不应截断: %q", got)
	}
	got := retriever.TruncatePageText(strings.Repeat("字", 31000))
	if n := len([]rune(got)); n != 30007 {
		t.Fatalf("want 30007 runes, got %d", n)
	}
	if !strings.HasSuffix(got, "…（已截断）") {
		t.Fatalf("截断尾标缺失")
	}
}
