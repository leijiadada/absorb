package obs

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	telemetrytrace "trpc.group/trpc-go/trpc-agent-go/telemetry/trace"

	"absorb-agent/internal/conf"
)

// TestFileExportRedactsMessages 落盘模式：span 写 traces.jsonl，且消息正文类
// 属性被剔除、超限的工具参数被截断（redactExporter 通道）。
func TestFileExportRedactsMessages(t *testing.T) {
	dir := t.TempDir()
	lc := conf.LogConfig{Dir: dir, MaxSizeMB: 10, MaxBackups: 1, MaxAgeDays: 1}
	if err := Init(conf.TelemetryConfig{Enabled: true}, lc); err != nil {
		t.Fatalf("Init: %v", err)
	}
	_, span := telemetrytrace.Tracer.Start(context.Background(), "chat obs-test-model")
	span.SetAttributes(
		attribute.String("gen_ai.input.messages", `{"secret":"prompt-body"}`),
		attribute.String("trpc.go.agent.llm_request", `{"secret":"full-request"}`),
		attribute.String("gen_ai.tool.call.result", `{"secret":"tool-result"}`),
		attribute.String("gen_ai.tool.call.arguments", strings.Repeat("x", 4000)),
		attribute.String("gen_ai.request.model", "obs-test-model"), // 非正文属性，应保留
	)
	span.End()
	Close()

	b, err := os.ReadFile(filepath.Join(dir, "traces.jsonl"))
	if err != nil {
		t.Fatalf("read traces.jsonl: %v", err)
	}
	s := string(b)
	for _, want := range []string{"chat obs-test-model", "obs-test-model"} {
		if !strings.Contains(s, want) {
			t.Errorf("traces.jsonl missing %q", want)
		}
	}
	for _, banned := range []string{"prompt-body", "full-request", "tool-result",
		"gen_ai.input.messages", "gen_ai.tool.call.result"} {
		if strings.Contains(s, banned) {
			t.Errorf("traces.jsonl leaks %q", banned)
		}
	}
	if strings.Contains(s, strings.Repeat("x", 4000)) {
		t.Errorf("tool.call.arguments not truncated")
	}
	if !strings.Contains(s, "...[truncated]") {
		t.Errorf("truncation marker missing")
	}
}

// TestDisabledNoop 关闭形态：不产生 traces.jsonl，全局保持 no-op。
func TestDisabledNoop(t *testing.T) {
	dir := t.TempDir()
	if err := Init(conf.TelemetryConfig{}, conf.LogConfig{Dir: dir}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	_, span := telemetrytrace.Tracer.Start(context.Background(), "chat obs-noop")
	span.End()
	Close()
	if _, err := os.Stat(filepath.Join(dir, "traces.jsonl")); !os.IsNotExist(err) {
		t.Errorf("traces.jsonl should not exist when disabled")
	}
}

// TestOTLPStartAgainstLocalJaeger OTLP 形态：本机 Jaeger 在跑时验证 Start/flush
// 全链路不报错（span 属性剔除走框架埋点层 policy，此处只验证连通性）。
func TestOTLPStartAgainstLocalJaeger(t *testing.T) {
	conn, err := net.DialTimeout("tcp", "localhost:4317", time.Second)
	if err != nil {
		t.Skip("jaeger not running on localhost:4317")
	}
	conn.Close()

	if err := Init(conf.TelemetryConfig{Enabled: true, Endpoint: "localhost:4317"},
		conf.LogConfig{Dir: t.TempDir()}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	_, span := telemetrytrace.Tracer.Start(context.Background(), "chat obs-otlp-test")
	span.SetAttributes(attribute.String("gen_ai.request.model", "obs-otlp-test"))
	span.End()
	Close() // shutdown flush OTLP export
}
