package obs

import (
	"database/sql"
	"encoding/json"
	"net/http"

	_ "embed" // go:embed traces.html 进 []byte（包未直接使用 embed 类型）
)

//go:embed static/traces.html
var tracesPageHTML []byte

// HTTP 链路查询端点与查看器（/debug/traces）。仅 telemetry sqlite 落库形态可用；
// 只读、无鉴权、无写操作——本地单用户调试页。
type HTTP struct{ db *sql.DB }

func newTraceHTTP(db *sql.DB) *HTTP { return &HTTP{db: db} }

// traceListEntry 列表条目：root 行即一条 trace 的锚点（parent_span_id 空串）。
type traceListEntry struct {
	TraceID    string `json:"traceId"`
	RootName   string `json:"rootName"`
	StartUs    int64  `json:"startUs"`
	DurationUs int64  `json:"durationUs"`
	SpanCount  int64  `json:"spanCount"`
	SessionID  string `json:"sessionId"`
	RequestID  string `json:"requestId"`
}

// Register 注册只读查询端点与查看器静态页（CLI mux 与 bridge Extra 共用一份实现）。
func (h *HTTP) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/traces", h.list)
	mux.HandleFunc("GET /api/traces/{traceID}", h.detail)
	mux.HandleFunc("GET /debug/traces", h.page)
}

func (h *HTTP) page(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(tracesPageHTML)
}

// list GET /api/traces：最近 50 条 trace（root 行 + span 总数子查询），新→旧。
func (h *HTTP) list(w http.ResponseWriter, _ *http.Request) {
	rows, err := h.db.Query(`SELECT r.trace_id, r.name, r.start_us, r.end_us, r.session_id, r.request_id,
		(SELECT COUNT(*) FROM request_traces s WHERE s.trace_id = r.trace_id) AS span_count
		FROM request_traces r WHERE r.parent_span_id = ''
		ORDER BY r.start_us DESC LIMIT 50`)
	if err != nil {
		writeTraceErr(w, err)
		return
	}
	defer rows.Close()
	traces := []traceListEntry{}
	for rows.Next() {
		var e traceListEntry
		var endUs int64
		if err := rows.Scan(&e.TraceID, &e.RootName, &e.StartUs, &endUs, &e.SessionID, &e.RequestID, &e.SpanCount); err != nil {
			writeTraceErr(w, err)
			return
		}
		e.DurationUs = endUs - e.StartUs
		traces = append(traces, e)
	}
	if err := rows.Err(); err != nil {
		writeTraceErr(w, err)
		return
	}
	writeTraceJSON(w, map[string]any{"traces": traces})
}

// traceSpanRow 详情行（attributes 原样透传 JSON）。
type traceSpanRow struct {
	SpanID       string          `json:"spanId"`
	ParentSpanID string          `json:"parentSpanId"`
	Name         string          `json:"name"`
	StartUs      int64           `json:"startUs"`
	DurationUs   int64           `json:"durationUs"`
	Status       string          `json:"status"`
	Attributes   json.RawMessage `json:"attributes"`
}

// detail GET /api/traces/{traceID}：整棵 span 树（按 start_us 升序，前端组树）。
func (h *HTTP) detail(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.Query(`SELECT span_id, parent_span_id, name, start_us, duration_us, status, attributes
		FROM request_traces WHERE trace_id = ? ORDER BY start_us, span_id`, r.PathValue("traceID"))
	if err != nil {
		writeTraceErr(w, err)
		return
	}
	defer rows.Close()
	spans := []traceSpanRow{}
	for rows.Next() {
		var s traceSpanRow
		var attrs string // TEXT 列先进 string，再转 RawMessage（直接 Scan RawMessage 驱动不支持）
		if err := rows.Scan(&s.SpanID, &s.ParentSpanID, &s.Name, &s.StartUs, &s.DurationUs, &s.Status, &attrs); err != nil {
			writeTraceErr(w, err)
			return
		}
		if attrs == "" {
			attrs = "{}"
		}
		s.Attributes = json.RawMessage(attrs)
		spans = append(spans, s)
	}
	if err := rows.Err(); err != nil {
		writeTraceErr(w, err)
		return
	}
	if len(spans) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "trace not found"})
		return
	}
	writeTraceJSON(w, map[string]any{"spans": spans})
}

func writeTraceErr(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func writeTraceJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
