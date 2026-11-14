// Package obs 生产可观测性装配：把 trpc-agent-go 框架的链路追踪（trace）接入 Absorb。
//
// 形态由 telemetry 配置段决定（enabled 总开关，默认关闭）：
//   - 关闭：no-op，框架保持 noop tracer，零开销；
//   - enabled 且 endpoint 为空、sqlite=false：span 落盘 <log.dir>/traces.jsonl
//     （json-lines，每行一个 span，lumberjack 轮转防膨胀），属性按 rules 剔除；
//   - enabled 且 endpoint 为空、sqlite=true：span 落会话库同级的 request_traces
//     表（SimpleSpanProcessor 同步落库），并开放 /debug/traces 查看器；
//   - enabled 且 endpoint 非空：走框架 telemetrytrace.Start 直发 OTLP
//     （如本机 Jaeger 的 gRPC 端口 4317），开发机临时观测用，不落盘。
//
// 正文边界：消息正文类属性（prompt/回答/工具结果/系统指令等）按 rules 剔除或
// 截断——正文已由 Debug 级全文日志与会话库承载，trace 只保留结构、时序与
// 少量定位字段（question/selected_text 截 2000 字符，其余经 request_id 回链）。
package obs

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace/noop"
	"gopkg.in/natefinch/lumberjack.v2"
	telemetrytrace "trpc.group/trpc-go/trpc-agent-go/telemetry/trace"

	"absorb-agent/internal/conf"
	"absorb-agent/internal/log"
)

// tracerInstrumentName 框架埋点的 instrumentation 名（internal/telemetry.InstrumentName 同值）。
const tracerInstrumentName = "trpc.agent.go"

// truncateMaxBytes 工具调用参数导出上限（超出截断：定位"模型为何这样调工具"
// 通常只需参数骨架，整页正文类参数不值得全量进 trace）。
const truncateMaxBytes = 2048

// shutdownTimeout Close 时 flush 剩余 span 的等待上限。
const shutdownTimeout = 5 * time.Second

// rule 单条 span 属性导出规则：truncate=false 为 Drop（不导出），true 为超限截断。
type rule struct {
	key      string
	truncate bool
}

// rules 属性导出规则（落盘过滤与 OTLP policy 共用一份真相源）：
// Drop 消息正文类属性——prompt/回答全文（gen_ai.input/output.messages 含 .otel
// 变体）、系统指令、工具返回结果、chat span 的 llm_request/llm_response 全文
// JSON（与 messages 同内容，不剔除则正文仍经它落盘）；工具调用参数截断。
var rules = []rule{
	{key: "gen_ai.input.messages"},
	{key: "gen_ai.input.messages.otel"},
	{key: "gen_ai.output.messages"},
	{key: "gen_ai.output.messages.otel"},
	{key: "gen_ai.system_instructions"},
	{key: "gen_ai.tool.call.result"},
	{key: "trpc.go.agent.llm_request"},
	{key: "trpc.go.agent.llm_response"},
	{key: "gen_ai.tool.call.arguments", truncate: true},
}

var (
	mu        sync.Mutex
	stop      func() error // 关停函数（flush + 恢复框架全局）；nil = 未开启
	traceHTTP *HTTP        // sqlite 形态的查看器（nil = 未启用）
)

// option 为 Init 的可选项（variadic 保持既有调用兼容）。
type option struct {
	sqliteDBPath string
}

// Option Init 可选项。
type Option func(*option)

// WithSQLiteStore 启用 sqlite 落库形态：span 写入该 SQLite 文件的 request_traces
// 表（表结构 DDL 与 store 共用 sqlite.TraceTablesDDL 单一文本源），并开放
// /debug/traces 查看器。仅在 enabled 且 endpoint 为空时生效。
func WithSQLiteStore(dbPath string) Option {
	return func(o *option) { o.sqliteDBPath = dbPath }
}

// Init 按配置接入链路追踪（须在启动 runner 之前调用，全局 Tracer 随后生效）。
// 失败返回错误；调用方建议降级为告警，不阻断服务启动。
func Init(cfg conf.TelemetryConfig, lc conf.LogConfig, opts ...Option) error {
	mu.Lock()
	defer mu.Unlock()
	if stop != nil {
		return errors.New("telemetry already initialized")
	}
	if !cfg.Enabled {
		log.Debugf("[Absorb] telemetry disabled")
		return nil
	}
	var o option
	for _, f := range opts {
		f(&o)
	}
	var (
		s   func() error
		err error
	)
	switch {
	case cfg.Endpoint != "":
		s, err = initOTLP(cfg)
	case o.sqliteDBPath != "":
		s, traceHTTP, err = initSQLiteExport(cfg, o.sqliteDBPath)
	default:
		s, err = initFileExport(cfg, lc)
	}
	if err != nil {
		return err
	}
	stop = s
	switch {
	case cfg.Endpoint != "":
		log.Infof("[Absorb] telemetry enabled: OTLP -> %s (service=%s)", cfg.Endpoint, serviceName(cfg))
	case o.sqliteDBPath != "":
		log.Infof("[Absorb] telemetry enabled: spans -> sqlite %s (viewer: /debug/traces)", o.sqliteDBPath)
	default:
		log.Infof("[Absorb] telemetry enabled: spans -> %s", filepath.Join(lc.Dir, "traces.jsonl"))
	}
	return nil
}

// TraceHTTP 返回 sqlite 形态的查看器（nil = 未启用）；调用方把 Register 挂上 mux。
func TraceHTTP() *HTTP {
	mu.Lock()
	defer mu.Unlock()
	return traceHTTP
}

// Close flush 并关停追踪（幂等；未开启时为 no-op）。进程退出前调用。
func Close() {
	mu.Lock()
	s := stop
	stop = nil
	traceHTTP = nil
	mu.Unlock()
	if s == nil {
		return
	}
	if err := s(); err != nil {
		log.Warnf("[Absorb] telemetry shutdown: %v", err)
	}
}

// initOTLP 走框架 telemetrytrace.Start：OTLP exporter + 框架侧属性 policy，
// Start 内部会替换 telemetry/trace 全局 Tracer 并注册 otel 全局。
func initOTLP(cfg conf.TelemetryConfig) (func() error, error) {
	clean, err := telemetrytrace.Start(context.Background(),
		telemetrytrace.WithEndpoint(cfg.Endpoint),
		telemetrytrace.WithServiceName(serviceName(cfg)),
		telemetrytrace.WithSpanAttributePolicy(policyOptions()...),
	)
	if err != nil {
		return nil, err
	}
	return clean, nil
}

// policyOptions 把 rules 翻译为框架 SpanAttributePolicy
// （空 operation 匹配全部 span 操作，语义与落盘过滤一致）。
func policyOptions() []telemetrytrace.AttributePolicyOption {
	out := make([]telemetrytrace.AttributePolicyOption, 0, len(rules))
	for _, r := range rules {
		if r.truncate {
			out = append(out, telemetrytrace.WithAttributeRule("",
				telemetrytrace.AttributeKey(r.key), telemetrytrace.Truncate(truncateMaxBytes)))
		} else {
			out = append(out, telemetrytrace.WithAttributeRule("",
				telemetrytrace.AttributeKey(r.key), telemetrytrace.Drop()))
		}
	}
	return out
}

// initSQLiteExport 落库形态：span 经 redact 过滤层写入 SQLite 的 request_traces 表。
// SimpleSpanProcessor 同步导出——本地单用户低频场景，落库即持久，无退出丢尾；
// exporter 打开连接时幂等兜底建表（obs.Init 先于 store 建表执行，消除时序依赖）。
// stop 闭包只做 provider Shutdown（SimpleSpanProcessor 在其中同步调 exporter 的
// Shutdown，连接随之关闭）；返回查看器供 main 挂路由。debug_port>0 时把查看器
// 额外固定挂 127.0.0.1:<port>（桌面桥端口动态分配，固定端口便于开发者收藏；
// 端口被占则告警降级——查看器仍可经业务 mux 的 /debug/traces 访问）。
func initSQLiteExport(cfg conf.TelemetryConfig, dbPath string) (func() error, *HTTP, error) {
	exp, err := newSQLiteExporter(dbPath)
	if err != nil {
		return nil, nil, err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(resource.NewWithAttributes(semconv.SchemaURL,
			semconv.ServiceNamespace("trpc-go-agent"),
			semconv.ServiceName(serviceName(cfg)),
		)),
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(newRedactExporter(exp))),
	)
	th := newTraceHTTP(exp.db)
	stopSrv := func() {}
	if cfg.DebugPort > 0 {
		addr := fmt.Sprintf("127.0.0.1:%d", cfg.DebugPort)
		mux := http.NewServeMux()
		th.Register(mux)
		srv, ln, serr := startDebugSrv(addr, mux)
		if serr != nil {
			log.Warnf("[Absorb] trace debug server %s unavailable (%v), viewer only on bridge mux /debug/traces", addr, serr)
		} else {
			log.Infof("[Absorb] trace viewer: http://%s/debug/traces", ln.Addr())
			stopSrv = func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_ = srv.Shutdown(ctx)
			}
		}
	}
	// 框架埋点经 telemetry/trace 包级变量发 span（与落盘形态同路，保证框架
	// gen_ai.* span 与业务 span 同树）；otel 全局与传播器一并对齐。
	telemetrytrace.TracerProvider = tp
	telemetrytrace.Tracer = tp.Tracer(tracerInstrumentName)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return func() error {
		stopSrv()
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		err := tp.Shutdown(ctx) // SimpleSpanProcessor 同步调 exporter.Shutdown 关库
		restoreNoop()
		return err
	}, th, nil
}

// startDebugSrv 起固定端口的本地查看器（仅绑 127.0.0.1，开发者用）。
// 同步 Listen：端口被占立即返回错误（调用方告警降级）；Serve 在后台运行。
func startDebugSrv(addr string, h http.Handler) (*http.Server, net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Warnf("[Absorb] trace debug server %s stopped: %v", addr, err)
		}
	}()
	return srv, ln, nil
}

// initFileExport 落盘模式：stdouttrace（json-lines）经 lumberjack 写 traces.jsonl，
// 前置一层属性过滤 exporter；手动注入框架 telemetry/trace 全局（不经过 Start，
// 属性规则由本包过滤层承担）。BatchSpanProcessor 聚合导出，Close 时统一 flush。
func initFileExport(cfg conf.TelemetryConfig, lc conf.LogConfig) (func() error, error) {
	if err := os.MkdirAll(lc.Dir, 0o755); err != nil {
		return nil, err
	}
	w := &lumberjack.Logger{
		Filename:   filepath.Join(lc.Dir, "traces.jsonl"),
		MaxSize:    lc.MaxSizeMB,
		MaxBackups: lc.MaxBackups,
		MaxAge:     lc.MaxAgeDays,
		LocalTime:  true,
	}
	exp, err := stdouttrace.New(stdouttrace.WithWriter(w))
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(resource.NewWithAttributes(semconv.SchemaURL,
			semconv.ServiceNamespace("trpc-go-agent"),
			semconv.ServiceName(serviceName(cfg)),
		)),
		sdktrace.WithSpanProcessor(sdktrace.NewBatchSpanProcessor(newRedactExporter(exp))),
	)
	// 框架埋点经 telemetry/trace 包级变量发 span（Start 之外的手动注入，
	// 与框架自测同路）；otel 全局与传播器一并对齐。
	telemetrytrace.TracerProvider = tp
	telemetrytrace.Tracer = tp.Tracer(tracerInstrumentName)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return func() error {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		err := tp.Shutdown(ctx) // BatchSpanProcessor flush 后关停
		restoreNoop()
		if cerr := w.Close(); err == nil { // 释放 lumberjack 文件句柄（Windows 必需）
			err = cerr
		}
		return err
	}, nil
}

// restoreNoop 把框架全局恢复为 no-op，防 Close 后再起的 span 写已关闭 provider。
func restoreNoop() {
	np := noop.NewTracerProvider()
	telemetrytrace.TracerProvider = np
	telemetrytrace.Tracer = np.Tracer("")
	otel.SetTracerProvider(np)
}

// serviceName resource service.name 有效值。
func serviceName(cfg conf.TelemetryConfig) string {
	if cfg.ServiceName != "" {
		return cfg.ServiceName
	}
	return "absorb"
}

// redactExporter 包装落盘 exporter：导出前按 rules 剔除/截断 span 属性。
type redactExporter struct{ next sdktrace.SpanExporter }

func newRedactExporter(next sdktrace.SpanExporter) sdktrace.SpanExporter {
	return &redactExporter{next: next}
}

func (e *redactExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	out := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, s := range spans {
		out[i] = s
		if attrs, changed := applyRules(s.Attributes()); changed {
			out[i] = filteredSpan{ReadOnlySpan: s, attrs: attrs}
		}
	}
	return e.next.ExportSpans(ctx, out)
}

func (e *redactExporter) Shutdown(ctx context.Context) error { return e.next.Shutdown(ctx) }

// filteredSpan 只覆写 Attributes（其余只读接口委托原 span），供 encoder 读到过滤后的属性。
type filteredSpan struct {
	sdktrace.ReadOnlySpan
	attrs []attribute.KeyValue
}

func (f filteredSpan) Attributes() []attribute.KeyValue { return f.attrs }

// applyRules 按 rules 过滤属性：Drop 直接剔除，Truncate 超限截断（追加截断标记）。
func applyRules(attrs []attribute.KeyValue) ([]attribute.KeyValue, bool) {
	out := make([]attribute.KeyValue, 0, len(attrs))
	changed := false
	for _, kv := range attrs {
		r := ruleFor(kv.Key)
		if r == nil {
			out = append(out, kv)
			continue
		}
		changed = true
		if !r.truncate {
			continue
		}
		s := kv.Value.AsString()
		if len(s) > truncateMaxBytes {
			kv = attribute.String(string(kv.Key), s[:truncateMaxBytes]+"...[truncated]")
		}
		out = append(out, kv)
	}
	return out, changed
}

// ruleFor 返回 key 对应的规则（nil = 无规则，原样导出）。
func ruleFor(k attribute.Key) *rule {
	for i := range rules {
		if rules[i].key == string(k) {
			return &rules[i]
		}
	}
	return nil
}
