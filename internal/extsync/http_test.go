package extsync

// 镜像同步端点：PUT 全量幂等、派生字段（title/msgCount）、未知字段零丢失、
// 体积与格式防御、404 分支、bridge.Deps.Extra 装配可达性。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"absorb-agent/internal/bridge"
	_ "absorb-agent/internal/store/sqlite" // sqlite 后端 init() 自注册（storage.New 依赖）
	"absorb-agent/internal/store/storage"
)

func newTestStore(t *testing.T) storage.Storage {
	t.Helper()
	st, err := storage.New("sqlite", filepath.Join(t.TempDir(), "extsync-test.db"))
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	NewHTTP(newTestStore(t), "app", "user").Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

const mirrorBody = `{
  "id": "ext-uid-1",
  "sessionId": "s1",
  "createdAt": 1730000000000,
  "updatedAt": 1730000100000,
  "quotes": [{ "id": 1, "text": "划词片段", "pageMd": "" }],
  "messages": [
    { "role": "user", "content": "什么是 SQLite 的 WAL 模式？它如何提升并发性能？这一段超长的问题文本用于验证标题截断逻辑是否生效", "ts": 1730000000000 },
    { "role": "assistant", "content": "WAL 允许读写并发。", "reasoning": "", "ts": 1730000100000 }
  ],
  "images": [{ "dataUrl": "data:image/jpeg;base64,xxx", "mime": "image/jpeg" }]
}`

func putMirror(t *testing.T, url, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	return resp
}

func TestPutAndRoundtrip(t *testing.T) {
	srv := newTestServer(t)

	resp := putMirror(t, srv.URL+"/api/ext/sessions/s1/mirror", mirrorBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put status = %d, want 200", resp.StatusCode)
	}

	// 索引：派生字段正确
	listResp, err := http.Get(srv.URL + "/api/ext/sessions")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	defer listResp.Body.Close()
	var list struct {
		Items []struct {
			SessionID string `json:"sessionId"`
			Title     string `json:"title"`
			MsgCount  int    `json:"msgCount"`
		} `json:"items"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(list.Items))
	}
	it := list.Items[0]
	if it.SessionID != "s1" || it.MsgCount != 2 {
		t.Fatalf("index item mismatch: %+v", it)
	}
	if !strings.HasPrefix(it.Title, "什么是 SQLite 的 WAL 模式") || len([]rune(it.Title)) > 61 {
		t.Fatalf("title not derived/truncated: %q", it.Title)
	}

	// 详情：未知字段（quotes/images/id/reasoning）零丢失
	detailResp, err := http.Get(srv.URL + "/api/ext/sessions/s1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer detailResp.Body.Close()
	if detailResp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want 200", detailResp.StatusCode)
	}
	var detail struct {
		SessionID string          `json:"sessionId"`
		Session   json.RawMessage `json:"session"`
	}
	if err := json.NewDecoder(detailResp.Body).Decode(&detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail.SessionID != "s1" {
		t.Fatalf("detail sessionId = %s", detail.SessionID)
	}
	for _, frag := range []string{`ext-uid-1`, `划词片段`, `reasoning`, `dataUrl`} {
		if !strings.Contains(string(detail.Session), frag) {
			t.Fatalf("session object lost field %s", frag)
		}
	}
}

func TestPutIdempotentOverwrite(t *testing.T) {
	srv := newTestServer(t)

	resp := putMirror(t, srv.URL+"/api/ext/sessions/s1/mirror", mirrorBody)
	resp.Body.Close()
	resp2 := putMirror(t, srv.URL+"/api/ext/sessions/s1/mirror",
		`{"createdAt":1730000000000,"updatedAt":1730000200000,"messages":[{"role":"user","content":"更新后的提问"}]}`)
	resp2.Body.Close()

	listResp, err := http.Get(srv.URL + "/api/ext/sessions")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	defer listResp.Body.Close()
	var list struct {
		Items []struct {
			SessionID string `json:"sessionId"`
			MsgCount  int    `json:"msgCount"`
		} `json:"items"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list.Items) != 1 || list.Items[0].MsgCount != 1 {
		t.Fatalf("overwrite mismatch: %+v", list.Items)
	}
}

func TestGetNotFound(t *testing.T) {
	srv := newTestServer(t)
	resp, err := http.Get(srv.URL + "/api/ext/sessions/no-such")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestPutRejectsBadBody(t *testing.T) {
	srv := newTestServer(t)

	resp := putMirror(t, srv.URL+"/api/ext/sessions/s1/mirror", `{invalid`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid json status = %d, want 400", resp.StatusCode)
	}

	huge := `{"messages":[],"pad":"` + strings.Repeat("x", maxMirrorBytes) + `"}`
	resp2 := putMirror(t, srv.URL+"/api/ext/sessions/s1/mirror", huge)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversize status = %d, want 400", resp2.StatusCode)
	}
}

// TestBridgeExtraRoutes 端到端：bridge.Deps.Extra 整组注册后镜像端点经桥可达，
// 堵 main.go 装配盲区（单测不覆盖路由装配）。
func TestBridgeExtraRoutes(t *testing.T) {
	b := bridge.New(0, "test", bridge.Deps{Extra: NewHTTP(newTestStore(t), "app", "user").Register})
	b.Start()
	defer b.Stop()
	if err := b.StartErr(); err != nil {
		t.Fatalf("bridge start: %v", err)
	}
	base := "http://localhost:" + strconv.Itoa(b.Port())

	resp := putMirror(t, base+"/api/ext/sessions/s1/mirror", mirrorBody)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put via bridge status = %d, want 200", resp.StatusCode)
	}

	getResp, err := http.Get(base + "/api/ext/sessions")
	if err != nil {
		t.Fatalf("list via bridge: %v", err)
	}
	defer getResp.Body.Close()
	body, _ := io.ReadAll(getResp.Body)
	if getResp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"s1"`) {
		t.Fatalf("list via bridge = %d %s", getResp.StatusCode, body)
	}
}
