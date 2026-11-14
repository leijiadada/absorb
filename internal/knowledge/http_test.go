package knowledge

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"absorb-agent/internal/bridge"
)

// newTestHTTP 起一个注册了知识端点的 httptest 服务（内存之外落临时 SQLite）。
// 预建来源会话 s1：M2.5 起保存链路有 L1 校验，createBody 里的 originSessionId 必须真实存在。
func newTestHTTP(t *testing.T) *httptest.Server {
	t.Helper()
	st := newTestStore(t)
	seedSession(t, st, "app", "user", "s1")
	svc := NewService(st)
	mux := http.NewServeMux()
	NewHTTP(svc, "app", "user").Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// postBody JSON POST 快捷方法，返回响应（调用方负责 Close）。
func postBody(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

// decode 解析 JSON 响应体到 map（同时关闭 body）。
func decode(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return m
}

const createBody = `{
  "title": "checkpoint 机制",
  "aiSummary": "WAL 通过 checkpoint 把日志并回主库",
  "originSessionId": "s1",
  "originEventId": "e1",
  "sources": [{"sourceTitle": "SQLite WAL 文档", "sourceUrl": "https://example.invalid/wal",
               "quoteText": "checkpoint 会把 WAL 中的内容写回数据库", "sourceSessionId": "s1", "sourceEventId": "e1"}]
}`

// TestHTTPCRUD 全链路：创建（201）→ 幂等命中（200, created=false）→ 列表 → 详情（含来源）
// → 编辑（改提炼不改引用）→ 归档 → 删除（幂等）→ 404。
func TestHTTPCRUD(t *testing.T) {
	srv := newTestHTTP(t)
	base := srv.URL

	// 校验错误 → 400（缺 title）
	resp := postBody(t, base+"/api/knowledge", `{"sources":[]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty title status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	resp = postBody(t, base+"/api/knowledge", createBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	m := decode(t, resp)
	if m["created"] != true {
		t.Fatalf("created = %v, want true", m["created"])
	}
	item := m["item"].(map[string]any)
	id := item["id"].(string)
	if id == "" {
		t.Fatalf("item id empty")
	}
	if _, leak := item["dedupKey"]; leak {
		t.Fatalf("dedupKey leaked to frontend")
	}

	// 幂等重复提交：200 + created=false + 同一条目
	resp = postBody(t, base+"/api/knowledge", createBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("idempotent create status = %d, want 200", resp.StatusCode)
	}
	m = decode(t, resp)
	if m["created"] != false {
		t.Fatalf("idempotent created = %v, want false", m["created"])
	}
	if dup := m["item"].(map[string]any)["id"]; dup != id {
		t.Fatalf("idempotent id = %v, want same %s", dup, id)
	}

	// 列表：active 有 1 条；非法 status 400
	resp, err := http.Get(base + "/api/knowledge?status=active")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	m = decode(t, resp)
	if len(m["items"].([]any)) != 1 {
		t.Fatalf("active items = %v, want 1", m["items"])
	}
	resp, err = http.Get(base + "/api/knowledge?status=bogus")
	if err != nil {
		t.Fatalf("list bogus: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bogus status status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// 详情含来源
	resp, err = http.Get(base + "/api/knowledge/" + id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	m = decode(t, resp)
	srcs := m["sources"].([]any)
	if len(srcs) != 1 {
		t.Fatalf("sources = %d, want 1", len(srcs))
	}
	quote := srcs[0].(map[string]any)["quoteText"].(string)
	if !strings.Contains(quote, "checkpoint") {
		t.Fatalf("quote = %q", quote)
	}

	// 详情 404
	resp, err = http.Get(base + "/api/knowledge/no-such-id")
	if err != nil {
		t.Fatalf("get missing: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()

	// 编辑：改提炼不改引用
	patch := `{"aiSummary": "用户改过的提炼", "title": "checkpoint 补充"}`
	req, _ := http.NewRequest(http.MethodPatch, base+"/api/knowledge/"+id, strings.NewReader(patch))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
	resp, _ = http.Get(base + "/api/knowledge/" + id)
	m = decode(t, resp)
	if m["item"].(map[string]any)["aiSummary"] != "用户改过的提炼" {
		t.Fatalf("aiSummary not updated")
	}
	if got := m["sources"].([]any)[0].(map[string]any)["quoteText"].(string); got != quote {
		t.Fatalf("quote changed after edit: %q", got)
	}

	// 归档 + 归档列表
	req, _ = http.NewRequest(http.MethodPatch, base+"/api/knowledge/"+id, strings.NewReader(`{"status":"archived"}`))
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	resp, _ = http.Get(base + "/api/knowledge?status=archived")
	m = decode(t, resp)
	if len(m["items"].([]any)) != 1 {
		t.Fatalf("archived items = %v, want 1", m["items"])
	}

	// 删除幂等 + 之后 404
	req, _ = http.NewRequest(http.MethodDelete, base+"/api/knowledge/"+id, nil)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()
	req, _ = http.NewRequest(http.MethodDelete, base+"/api/knowledge/"+id, nil)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("idempotent delete status = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()
	resp, _ = http.Get(base + "/api/knowledge/" + id)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("after delete status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()

	// 方法不符 → 405（ServeMux 自动带 Allow）
	resp, err = http.Post(base+"/api/knowledge/"+id, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("post to {id}: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("method mismatch status = %d, want 405", resp.StatusCode)
	}
}

// TestHTTPCreateRejectsUnknownSession L1 校验：origin 会话不存在 → 400 + 固定中文提示。
func TestHTTPCreateRejectsUnknownSession(t *testing.T) {
	srv := newTestHTTP(t)
	body := strings.Replace(createBody, `"originSessionId": "s1"`, `"originSessionId": "ghost"`, 1)
	resp := postBody(t, srv.URL+"/api/knowledge", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown origin session status = %d, want 400", resp.StatusCode)
	}
	m := decode(t, resp)
	if msg, _ := m["message"].(string); !strings.Contains(msg, "来源会话不存在") {
		t.Fatalf("message = %q, want 来源会话不存在提示", msg)
	}
}

// TestHTTPSaveSummary 阅读摘要端点（M2.5 层次A）：新建 201 → 幂等 200 →
// 未知来源会话 400 → 缺必填 400。保存摘要永不产生知识条目。
func TestHTTPSaveSummary(t *testing.T) {
	srv := newTestHTTP(t)
	base := srv.URL
	summaryBody := `{"sessionId":"s1","requestId":"r1","url":"https://example.invalid/wal",
		"title":"SQLite WAL","summaryText":"WAL 把写放大摊平成顺序追加"}`

	resp := postBody(t, base+"/api/reading-summary", summaryBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("save summary status = %d, want 201", resp.StatusCode)
	}
	m := decode(t, resp)
	if m["created"] != true {
		t.Fatalf("created = %v, want true", m["created"])
	}
	docID, _ := m["documentId"].(string)
	if docID == "" {
		t.Fatal("documentId empty")
	}

	// 同一回答重复提交：200 + created=false + 同一文档
	resp = postBody(t, base+"/api/reading-summary", summaryBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("idempotent summary status = %d, want 200", resp.StatusCode)
	}
	m = decode(t, resp)
	if m["created"] != false {
		t.Fatalf("idempotent created = %v, want false", m["created"])
	}
	if dup := m["documentId"]; dup != docID {
		t.Fatalf("idempotent documentId = %v, want %s", dup, docID)
	}

	// 未知来源会话 → 400
	resp = postBody(t, base+"/api/reading-summary",
		`{"sessionId":"ghost","requestId":"r2","url":"https://example.invalid/x","summaryText":"t"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown session status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// 缺 url → 400
	resp = postBody(t, base+"/api/reading-summary",
		`{"sessionId":"s1","requestId":"r3","summaryText":"t"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing url status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// 确认未产生知识条目（总结与吸收是两类行为）；空列表序列化为 null
	list, err := http.Get(base + "/api/knowledge?status=active")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	items, _ := decode(t, list)["items"].([]any)
	if len(items) != 0 {
		t.Fatalf("summary must not create knowledge items, got %d", len(items))
	}
}

// TestHTTPOPTIONSPreflight 预检放行 PATCH/DELETE 且带正确方法集。
func TestHTTPOPTIONSPreflight(t *testing.T) {
	srv := newTestHTTP(t)
	req, _ := http.NewRequest(http.MethodOptions, srv.URL+"/api/knowledge", nil)
	req.Header.Set("Access-Control-Request-Method", "PATCH")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", resp.StatusCode)
	}
	if methods := resp.Header.Get("Access-Control-Allow-Methods"); !strings.Contains(methods, "PATCH") || !strings.Contains(methods, "DELETE") {
		t.Fatalf("allow-methods = %q", methods)
	}
}

// TestHTTPEventsSSE 订阅 SSE 后做 创建→编辑→删除，按序收到三种事件；
// 幂等命中不发事件。订阅先于写入建立（服务端 Flush 后客户端才拿到响应头），
// 因此时序确定无竞态。
func TestHTTPEventsSSE(t *testing.T) {
	srv := newTestHTTP(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/knowledge/events", nil)
	// evResp 独立变量：后续 CRUD 请求不得覆盖，读流始终来自订阅响应
	evResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer evResp.Body.Close()
	if ct := evResp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}

	// 创建 → 幂等重复提交（静默，不发事件）→ 编辑 → 删除
	var resp *http.Response
	resp = postBody(t, srv.URL+"/api/knowledge", createBody)
	id := decode(t, resp)["item"].(map[string]any)["id"].(string)
	if resp = postBody(t, srv.URL+"/api/knowledge", createBody); resp.StatusCode != http.StatusOK {
		t.Fatalf("idempotent status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
	req, _ = http.NewRequest(http.MethodPatch, srv.URL+"/api/knowledge/"+id, strings.NewReader(`{"aiSummary":"改"}`))
	if resp, err = http.DefaultClient.Do(req); err != nil {
		t.Fatalf("patch: %v", err)
	}
	resp.Body.Close()
	req, _ = http.NewRequest(http.MethodDelete, srv.URL+"/api/knowledge/"+id, nil)
	if resp, err = http.DefaultClient.Do(req); err != nil {
		t.Fatalf("delete: %v", err)
	}
	resp.Body.Close()

	// 读流：期望恰好 3 帧，类型与顺序确定
	reader := bufio.NewReader(evResp.Body)
	want := []string{EventCreated, EventUpdated, EventDeleted}
	for _, w := range want {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read event %q: %v (frames may be missing/dropped)", w, err)
		}
		if line == "\n" { // SSE 帧以空行收尾，跳过
			line, err = reader.ReadString('\n')
			if err != nil {
				t.Fatalf("read event %q after blank: %v", w, err)
			}
		}
		if !strings.HasPrefix(line, "data: ") {
			t.Fatalf("unexpected line %q", line)
		}
		var ev KnowledgeEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("unmarshal event: %v", err)
		}
		if ev.Kind != w {
			t.Fatalf("event kind = %q, want %q", ev.Kind, w)
		}
		if ev.Item == nil {
			t.Fatalf("event %s missing item", w)
		}
	}
	// 幂等提交的静默验证：短暂等待后流上不应再有数据帧
	extra := make(chan string, 1)
	go func() {
		for {
			line, _ := reader.ReadString('\n')
			if line != "\n" { // 跳过帧尾空行，只认数据行
				extra <- line
				return
			}
		}
	}()
	select {
	case line := <-extra:
		t.Fatalf("unexpected extra event frame: %q", line)
	case <-time.After(300 * time.Millisecond):
	}
	cancel()
	// 丢弃剩余读取错误（ctx 取消所致），确保 body 已关闭
	_, _ = io.Copy(io.Discard, evResp.Body)
}

// TestBridgeExtraRoutes 端到端：bridge.Deps.Extra 整组注册后知识端点经桥可达，
// 堵 main.go 装配盲区（单测不覆盖路由装配）。
func TestBridgeExtraRoutes(t *testing.T) {
	svc := NewService(newTestStore(t))
	b := bridge.New(0, "test", bridge.Deps{Extra: NewHTTP(svc, "app", "user").Register})
	b.Start()
	defer b.Stop()
	if err := b.StartErr(); err != nil {
		t.Fatalf("bridge start: %v", err)
	}
	resp, err := http.Get("http://localhost:" + strconv.Itoa(b.Port()) + "/api/knowledge?status=active")
	if err != nil {
		t.Fatalf("list via bridge: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list via bridge status = %d, want 200", resp.StatusCode)
	}
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := m["items"]; !ok {
		t.Fatalf("missing items field: %v", m)
	}
}
