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
// 本例 documentId=URL（M0 前旧扩展卡片语义），守卫问答链路「按 ID 查无回退
// 按 URL」的兼容窗口。UUID 主路径见 TestDocumentIdUUIDInjectsPageContent。
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
	// 文档轮不写页面缓存（expand_context 为网页划词专属；文档原文阅读走
	// search_document 读页模式）
	if got := loadPageCache(t, svc, sid, url); got != "" {
		t.Fatalf("文档轮不应写页面缓存，got %q", got)
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

// 文档注入（M0 统一身份主路径）：documentId = documents.id UUID，按 ID 直接
// 命中 document_contents（JOIN documents），无需 URL 回退。
func TestDocumentIdUUIDInjectsPageContent(t *testing.T) {
	h, cap2, svc := setup(t, "openai", true, false, defaultOKStream)
	const sid = "s-doc-uuid"
	const url = "https://paper.example.com/uuid-doc.pdf"
	doc, err := svc.Storage().UpsertDocument(context.Background(), AppName, UserID, url, "论文")
	if err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	dc := &storage.DocumentContent{
		URL:              url,
		ContentText:      "UUID 主路径正文：文档身份统一后的注入验证",
		CharCount:        22,
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
		"page":      map[string]any{"url": url, "title": "论文", "documentId": doc.ID},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/ask", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, req)
	if strings.Contains(rec.Body.String(), `"type":"error"`) {
		t.Fatalf("请求不应报错，SSE: %s", rec.Body.String())
	}
	// 文档轮不写页面缓存（expand_context 为网页划词专属）
	if got := loadPageCache(t, svc, sid, url); got != "" {
		t.Fatalf("文档轮不应写页面缓存，got %q", got)
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
	if !strings.Contains(userMsg, "<page_content>") || !strings.Contains(userMsg, "UUID 主路径正文") {
		t.Fatalf("UUID documentId 应注入 <page_content>，user message: %q", userMsg)
	}
}

// 文档划词（PDF 解读联调回归）：划词来源是文档页时，<page_content> 仍须注入
// （划词只是文档一小段，全文才是回答关键）且划词材料不丢、
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

// docTypeOf 纯函数守卫：ContentType 权威，URL 后缀兜底，其余落网页口径。
func TestDocTypeOf(t *testing.T) {
	cases := []struct {
		ct, url, want string
	}{
		{"application/pdf", "https://x.example.com/a", "pdf"},
		{"APPLICATION/PDF ", "https://x.example.com/a", "pdf"},
		{"text/html", "https://x.example.com/b.pdf", "pdf"},
		{"text/html", "https://x.example.com/b.html", "web"},
		{"", "", "web"},
	}
	for _, c := range cases {
		if got := string(docTypeOf(c.ct, c.url)); got != c.want {
			t.Fatalf("docTypeOf(%q,%q) = %q, want %q", c.ct, c.url, got, c.want)
		}
	}
}

// 文档轮尾注守卫（DB 取证回归，会话 fc2c86e0 seq16：模型对"划词只是一句小标题"
// 不敢检索、也未意识到 search_document 可用）：正文超 6000 字截断时，尾注须引导
// search_document 且点破划词标题可作查询词，不得再引导 read_page（文档轮页面缓存
// 为空，调用必失败）；来源行显式标注 PDF。
func TestDocumentTailGuidesSearchDocument(t *testing.T) {
	h, cap2, svc := setup(t, "openai", true, false, defaultOKStream)
	const sid = "s-doc-tail"
	const url = "https://paper.example.com/agenticse-tail.pdf"
	dc := &storage.DocumentContent{
		URL:              url,
		ContentText:      strings.Repeat("章节正文。", 1500), // 7500 runes，超 6000 触发截断尾注
		CharCount:        7500,
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
	if !strings.Contains(userMsg, "来源："+url+"（PDF 文档）") {
		t.Fatalf("来源行应标注 PDF 文档，user message: %q", userMsg)
	}
	if !strings.Contains(userMsg, "search_document 工具按关键词检索") || !strings.Contains(userMsg, "划词标题") {
		t.Fatalf("文档轮尾注应引导 search_document 并点破划词标题可作查询词，user message: %q", userMsg)
	}
	if strings.Contains(userMsg, "read_page 工具查看章节目录") {
		t.Fatalf("文档轮不应引导 read_page（页面缓存为空，必失败），user message: %q", userMsg)
	}
}

// 查看器地址安全网（存量脏数据回归）：划词 URL 是内置查看器扩展页地址且 page.url
// 为正常网页时，服务端改用浏览页 URL——<page_url>/来源行/scope 归组全部纠正，
// 无需等待扩展端更新。
func TestViewerQuoteURLFallsBackToPageURL(t *testing.T) {
	h, cap2, svc := setup(t, "openai", true, false, defaultOKStream)
	const sid = "s-viewer-url"
	const realURL = "https://paper.example.com/agenticse-real.pdf"
	const viewerURL = "chrome-extension://mhjfbmdgcfjbbpaeojofohoefgiehjai/edge_pdf/index.html"
	dc := &storage.DocumentContent{
		URL:              realURL,
		ContentText:      "安全网回归正文",
		CharCount:        6,
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
			"text": "核心概念：策略即代码",
			"url":  viewerURL, // 存量脏数据：采集侧纠正未生效
		},
		"page": map[string]any{"url": realURL, "title": "论文", "documentId": realURL},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/ask", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, req)
	if strings.Contains(rec.Body.String(), `"type":"error"`) {
		t.Fatalf("请求不应报错，SSE: %s", rec.Body.String())
	}
	// scope 归组落到真实文档 URL，而不是查看器扩展页地址
	if got, err := svc.RecentPageURL(context.Background(), AppName, UserID, sid); err != nil || got != realURL {
		t.Fatalf("scope url 应为真实文档地址，got %q err=%v", got, err)
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
	if strings.Contains(userMsg, viewerURL) {
		t.Fatalf("prompt 不应残留查看器扩展页地址，user message: %q", userMsg)
	}
	if !strings.Contains(userMsg, "<page_url>\n"+realURL+"\n</page_url>") {
		t.Fatalf("<page_url> 应渲染真实文档地址，user message: %q", userMsg)
	}
	if !strings.Contains(userMsg, "来源："+realURL+"（PDF 文档）") {
		t.Fatalf("来源行应为真实地址并标注 PDF，user message: %q", userMsg)
	}
}

// 网页轮尾注守卫：无划词但带 pageMd（正文超 6000 截断）时，尾注保持 read_page
// 引导原文案，不得渲染 PDF 标注与 search_document 引导（prompt 与历史逐字节一致）。
func TestWebPageMdTailKeepsReadPageGuide(t *testing.T) {
	h, cap2, _ := setup(t, "openai", true, false, defaultOKStream)
	const sid = "s-web-tail"
	body, _ := json.Marshal(map[string]any{
		"question":  "这页讲什么？",
		"sessionId": sid,
		"context": map[string]any{
			"pageMd": strings.Repeat("网页正文段落。", 1100), // 6600 runes，超 6000 触发截断
		},
		"page": map[string]any{"url": "https://md-test.com/d", "title": "文章"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/ask", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, req)
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
	if !strings.Contains(userMsg, "read_page 工具查看章节目录") {
		t.Fatalf("网页轮尾注应保持 read_page 引导原文案，user message: %q", userMsg)
	}
	if strings.Contains(userMsg, "（PDF 文档）") || strings.Contains(userMsg, "search_document 工具按关键词检索") {
		t.Fatalf("网页轮不应渲染 PDF 标注与 search_document 引导，user message: %q", userMsg)
	}
}
