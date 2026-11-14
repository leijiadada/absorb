package obs

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// TestTraceHTTPListAndDetail 查看器端到端（走真实 mux，PathValue 依赖路由模式）：
// 列表只出 root 行、详情含全部 span 且父子关联、未知 trace 404、静态页 200。
func TestTraceHTTPListAndDetail(t *testing.T) {
	exp := seedSQLiteTrace(t, filepath.Join(t.TempDir(), "trace-http-test.db"))
	mux := http.NewServeMux()
	newTraceHTTP(exp.db).Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	fetch := func(path string) (int, string, http.Header) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return resp.StatusCode, string(b), resp.Header
	}

	// 列表：只有 root（ask）一行，span_count=2，归属列回填
	code, body, _ := fetch("/api/traces")
	if code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", code, body)
	}
	var listResp struct {
		Traces []traceListEntry `json:"traces"`
	}
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("list json: %v (%s)", err, body)
	}
	if len(listResp.Traces) != 1 {
		t.Fatalf("want 1 trace, got %d (%s)", len(listResp.Traces), body)
	}
	e := listResp.Traces[0]
	if e.RootName != "ask" || e.SpanCount != 2 || e.SessionID != "sess-1" || e.RequestID != "req-1" {
		t.Errorf("list entry wrong: %+v", e)
	}

	// 详情：2 个 span，父子关联正确
	code, body, _ = fetch("/api/traces/" + e.TraceID)
	if code != http.StatusOK {
		t.Fatalf("detail status = %d body=%s", code, body)
	}
	var detailResp struct {
		Spans []traceSpanRow `json:"spans"`
	}
	if err := json.Unmarshal([]byte(body), &detailResp); err != nil {
		t.Fatalf("detail json: %v (%s)", err, body)
	}
	if len(detailResp.Spans) != 2 {
		t.Fatalf("want 2 spans, got %d (%s)", len(detailResp.Spans), body)
	}
	var root, child *traceSpanRow
	for i := range detailResp.Spans {
		switch detailResp.Spans[i].Name {
		case "ask":
			root = &detailResp.Spans[i]
		case "retrieval.associations":
			child = &detailResp.Spans[i]
		}
	}
	if root == nil || child == nil {
		t.Fatalf("missing spans: %+v", detailResp.Spans)
	}
	if child.ParentSpanID != root.SpanID {
		t.Errorf("child.ParentSpanID = %q, want root %q", child.ParentSpanID, root.SpanID)
	}
	if len(root.Attributes) == 0 {
		t.Errorf("root attributes empty")
	}

	// 未知 trace：404
	if code, _, _ = fetch("/api/traces/no-such-trace"); code != http.StatusNotFound {
		t.Errorf("missing trace status = %d, want 404", code)
	}

	// 静态查看器页：200 + html
	code, _, hdr := fetch("/debug/traces")
	if code != http.StatusOK || hdr.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Errorf("page status=%d content-type=%q", code, hdr.Get("Content-Type"))
	}
}

// TestDebugSrvBindsAndServes startDebugSrv：同步 bind、后台 serve，路由可达。
func TestDebugSrvBindsAndServes(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	srv, ln, err := startDebugSrv("127.0.0.1:0", mux)
	if err != nil {
		t.Fatalf("startDebugSrv: %v", err)
	}
	defer func() { _ = srv.Shutdown(context.Background()) }()
	resp, err := http.Get("http://" + ln.Addr().String() + "/ping")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

// TestDebugSrvPortBusy 端口被占：同步报错（调用方告警降级），不产生半启动 server。
func TestDebugSrvPortBusy(t *testing.T) {
	ln1, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("listen: %v", err)
	}
	defer ln1.Close()
	if _, _, err = startDebugSrv(ln1.Addr().String(), http.NewServeMux()); err == nil {
		t.Fatalf("second bind on %s should fail", ln1.Addr())
	}
}
