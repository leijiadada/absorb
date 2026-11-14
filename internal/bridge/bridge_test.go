package bridge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestBridge 构造未监听的桥（port=0）。测试直接驱动其 Handler，
// 既不占用真实端口，也不必依赖 Start 的 serve goroutine。
func newTestBridge(t *testing.T, deps Deps) *Bridge {
	t.Helper()
	return New(0, "v-test", deps)
}

// TestHealth 冒烟探测端点：返回服务特征字段，供外部确认服务就绪。
func TestHealth(t *testing.T) {
	b := newTestBridge(t, Deps{})

	rec := httptest.NewRecorder()
	b.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("/health 状态码 = %d, want 200", rec.Code)
	}
	var got struct {
		Service string `json:"service"`
		Version string `json:"version"`
		OK      bool   `json:"ok"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析 /health 响应失败: %v (body=%s)", err, rec.Body.String())
	}
	if got.Service != "absorb-agent" || got.Version != "v-test" || !got.OK {
		t.Errorf("/health = %+v, want service=absorb-agent version=v-test ok=true", got)
	}
}

// TestExtConnectDisconnect Native 管道建立/断开：同 PID 重报幂等，
// 计数归零才记断开时刻（Edge/Chrome 并存时任一断开不误判）。
func TestExtConnectDisconnect(t *testing.T) {
	b := newTestBridge(t, Deps{})

	post := func(path, body string) int {
		t.Helper()
		rec := httptest.NewRecorder()
		b.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		return rec.Code
	}

	// 首次建立
	if code := post("/ext/connect", `{"pid":123}`); code != http.StatusNoContent {
		t.Fatalf("connect 状态码 = %d, want 204", code)
	}
	if _, conns, disc := b.ExtLink(); conns != 1 || disc != 0 {
		t.Fatalf("建立后 conns=%d disconnectAt=%d, want conns=1 disconnectAt=0", conns, disc)
	}

	// 同 PID 重复上报（宿主周期重报）必须幂等，不重复计数
	if code := post("/ext/connect", `{"pid":123}`); code != http.StatusNoContent {
		t.Fatalf("重复 connect 状态码 = %d, want 204", code)
	}
	if _, conns, _ := b.ExtLink(); conns != 1 {
		t.Fatalf("同 PID 重报后 conns=%d, want 1（幂等）", conns)
	}

	// 不同 PID 各自计数
	if code := post("/ext/connect", `{"pid":456}`); code != http.StatusNoContent {
		t.Fatalf("第二个 PID connect 状态码 = %d, want 204", code)
	}
	if _, conns, _ := b.ExtLink(); conns != 2 {
		t.Fatalf("第二个 PID 建立后 conns=%d, want 2", conns)
	}

	// 任一断开：计数减一，但不记断开时刻
	if code := post("/ext/disconnect", `{"pid":123}`); code != http.StatusNoContent {
		t.Fatalf("disconnect 状态码 = %d, want 204", code)
	}
	if _, conns, disc := b.ExtLink(); conns != 1 || disc != 0 {
		t.Fatalf("单条断开后 conns=%d disconnectAt=%d, want conns=1 disconnectAt=0", conns, disc)
	}

	// 全部断开：计数归零并记断开时刻
	if code := post("/ext/disconnect", `{"pid":456}`); code != http.StatusNoContent {
		t.Fatalf("最后 disconnect 状态码 = %d, want 204", code)
	}
	if _, conns, disc := b.ExtLink(); conns != 0 || disc == 0 {
		t.Fatalf("全部断开后 conns=%d disconnectAt=%d, want conns=0 disconnectAt>0", conns, disc)
	}
}

// TestCorsPreflight 浏览器扩展跨域预检（OPTIONS）应返回 204 与 CORS 头。
func TestCorsPreflight(t *testing.T) {
	noop := func(w http.ResponseWriter, r *http.Request) {}
	b := newTestBridge(t, Deps{Ask: noop})

	rec := httptest.NewRecorder()
	b.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodOptions, "/api/ask", nil))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("OPTIONS /api/ask = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("CORS Allow-Origin = %q, want *", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, "POST") {
		t.Errorf("CORS Allow-Methods = %q, want 含 POST", got)
	}
}

// TestMethodNotAllowed method-aware 路由：方法不符由 ServeMux 返回 405，并带 Allow 头。
func TestMethodNotAllowed(t *testing.T) {
	noop := func(w http.ResponseWriter, r *http.Request) {}
	b := newTestBridge(t, Deps{Ask: noop})

	rec := httptest.NewRecorder()
	b.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/ask", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/ask = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got == "" {
		t.Errorf("405 响应缺少 Allow 头")
	}
}

// TestStartPortBackfill Start 同步监听后回填实际端口（port=0 时由 OS 分配），
// 且启动无错误；Stop 负责优雅停机，避免 serve goroutine 泄漏到下一个测试。
func TestStartPortBackfill(t *testing.T) {
	b := newTestBridge(t, Deps{})
	b.Start()
	t.Cleanup(b.Stop)

	if p := b.Port(); p <= 0 {
		t.Fatalf("Start 后 Port() = %d, want > 0（OS 分配端口应回填）", p)
	}
	if err := b.StartErr(); err != nil {
		t.Fatalf("StartErr 不应有错误: %v", err)
	}
}
