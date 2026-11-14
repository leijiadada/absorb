package obs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"absorb-agent/internal/store/sqlite"
)

// sqliteSpanExporter 把 SDK 导出的 span 批量写入 request_traces 表。
// 独立数据库连接（WAL 多连接安全）；表结构由 store 包 TraceTablesDDL
// 单一文本源保证，Open 时兜底建表（幂等）——正常时序由 store 迁移负责，
// 此处保证 exporter 先于 store 初始化也能落库。
type sqliteSpanExporter struct{ db *sql.DB }

func newSQLiteExporter(dbPath string) (*sqliteSpanExporter, error) {
	// modernc 纯 Go 驱动（store 同款 pragma：WAL + busy 超时，多连接写安全）
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open trace store: %w", err)
	}
	if _, err := db.Exec(sqlite.TraceTablesDDL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ensure trace table: %w", err)
	}
	return &sqliteSpanExporter{db: db}, nil
}

const insertSpanSQL = `INSERT OR REPLACE INTO request_traces
	(trace_id, span_id, parent_span_id, name, start_us, end_us, duration_us,
	 status, attributes, app_name, user_id, session_id, request_id)
	VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`

// ExportSpans 单事务批量落库。失败上抛给 SDK（otel 全局错误处理器记录），
// 观测链路永不阻断业务。
func (e *sqliteSpanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	if len(spans) == 0 {
		return nil
	}
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, insertSpanSQL)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, s := range spans {
		sc := s.SpanContext()
		parent := ""
		if p := s.Parent(); p.IsValid() {
			parent = p.SpanID().String()
		}
		a := extractAttrs(s.Attributes())
		start, end := s.StartTime(), s.EndTime()
		if _, err := stmt.ExecContext(ctx,
			sc.TraceID().String(), sc.SpanID().String(), parent, s.Name(),
			start.UnixMicro(), end.UnixMicro(), end.Sub(start).Microseconds(),
			strings.ToLower(s.Status().Code.String()), a.json, a.app, a.user, a.session, a.request,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Shutdown 断开连接（provider.Shutdown 经 processor 调用，flush 在先）。
func (e *sqliteSpanExporter) Shutdown(ctx context.Context) error { return e.db.Close() }

// extractedAttrs 落库行字段：attributes JSON + 四个从属性提取的归属列。
type extractedAttrs struct {
	json, app, user, session, request string
}

// extractAttrs 序列化属性为 JSON（redactExporter 已在更早一环剔除消息正文，
// 此处收到的属性均已过滤），同时把归属字段提取为独立列（键与 chat/handler.go
// 的 ask span 埋点约定一致：app_name/user_id/session_id/request_id）。
// 注意按 Value 类型取原生 Go 值：Emit() 会把切片/布尔一律转成字符串形态，
// 落库后查看器无法区分结构与字面量。
func extractAttrs(kvs []attribute.KeyValue) extractedAttrs {
	m := make(map[string]any, len(kvs))
	var out extractedAttrs
	for _, kv := range kvs {
		var v any
		switch kv.Value.Type() {
		case attribute.BOOL:
			v = kv.Value.AsBool()
		case attribute.INT64:
			v = kv.Value.AsInt64()
		case attribute.FLOAT64:
			v = kv.Value.AsFloat64()
		case attribute.BOOLSLICE:
			v = kv.Value.AsBoolSlice()
		case attribute.INT64SLICE:
			v = kv.Value.AsInt64Slice()
		case attribute.FLOAT64SLICE:
			v = kv.Value.AsFloat64Slice()
		case attribute.STRINGSLICE:
			v = kv.Value.AsStringSlice()
		default:
			v = kv.Value.AsString()
		}
		m[string(kv.Key)] = v
		switch string(kv.Key) {
		case "app_name":
			out.app = kv.Value.String()
		case "user_id":
			out.user = kv.Value.String()
		case "session_id":
			out.session = kv.Value.String()
		case "request_id":
			out.request = kv.Value.String()
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		b = []byte("{}")
	}
	out.json = string(b)
	return out
}
