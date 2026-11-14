package obs

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// seedSQLiteTrace 用本地 provider（WithSyncer 同步导出，redact 过滤层在链路中）
// 造一条 root+child trace 落库，返回 exporter 供断言；provider 关停（含关库）
// 挂 t.Cleanup，在测试函数的 defer 之后执行。
func seedSQLiteTrace(t *testing.T, dbPath string) *sqliteSpanExporter {
	t.Helper()
	exp, err := newSQLiteExporter(dbPath)
	if err != nil {
		t.Fatalf("newSQLiteExporter: %v", err)
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(newRedactExporter(exp)))
	ctx, root := tp.Tracer("test").Start(context.Background(), "ask")
	root.SetAttributes(
		attribute.String("app_name", "absorb"),
		attribute.String("user_id", "u1"),
		attribute.String("session_id", "sess-1"),
		attribute.String("request_id", "req-1"),
		attribute.String("gen_ai.input.messages", `{"secret":"SECRET-PROMPT-BODY"}`),
	)
	_, child := tp.Tracer("test").Start(ctx, "retrieval.associations")
	child.SetAttributes(
		attribute.StringSlice("keywords", []string{"Runner", "多Agent"}),
		attribute.Bool("zero_hit", true),
	)
	child.End()
	root.End()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return exp
}

// TestSQLiteExporterWritesTrace 根+子 span 落 request_traces：父子关联、归属列
// 提取、消息正文剔除三层语义一次验证。
func TestSQLiteExporterWritesTrace(t *testing.T) {
	exp := seedSQLiteTrace(t, filepath.Join(t.TempDir(), "trace-test.db"))

	var n int
	if err := exp.db.QueryRow(`SELECT COUNT(*) FROM request_traces`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("want 2 rows, got %d", n)
	}

	var rootTrace, rootSpan, rootParent, rootAttrs, rootSession, rootRequest, rootStatus string
	if err := exp.db.QueryRow(`SELECT trace_id, span_id, parent_span_id, attributes, session_id, request_id, status
		FROM request_traces WHERE name='ask'`).
		Scan(&rootTrace, &rootSpan, &rootParent, &rootAttrs, &rootSession, &rootRequest, &rootStatus); err != nil {
		t.Fatalf("query root: %v", err)
	}
	if rootParent != "" {
		t.Errorf("root parent_span_id = %q, want empty", rootParent)
	}
	if rootStatus != "unset" { // 小写文本，与 DDL 默认值一致（规避 otel 数值映射歧义）
		t.Errorf("root status = %q, want unset", rootStatus)
	}
	if rootSession != "sess-1" || rootRequest != "req-1" {
		t.Errorf("归属列未提取: session=%q request=%q", rootSession, rootRequest)
	}
	// redact 层语义：消息正文与其属性键都不得出现在落库 JSON 中
	if strings.Contains(rootAttrs, "SECRET-PROMPT-BODY") || strings.Contains(rootAttrs, "gen_ai.input.messages") {
		t.Errorf("root attributes leak prompt body: %s", rootAttrs)
	}

	var childTrace, childParent, childAttrs string
	if err := exp.db.QueryRow(`SELECT trace_id, parent_span_id, attributes
		FROM request_traces WHERE name='retrieval.associations'`).
		Scan(&childTrace, &childParent, &childAttrs); err != nil {
		t.Fatalf("query child: %v", err)
	}
	if childTrace != rootTrace || childParent != rootSpan {
		t.Errorf("child not linked: trace %q vs %q, parent %q vs %q", childTrace, rootTrace, childParent, rootSpan)
	}
	if !strings.Contains(childAttrs, "Runner") || !strings.Contains(childAttrs, `"zero_hit":true`) {
		t.Errorf("child attributes missing keywords/zero_hit: %s", childAttrs)
	}
}
