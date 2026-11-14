// absorb-eval M0 评测入口。
//
// 与生产共用公共层（conf.Load → chat.NewRunner → chat.PrepareInvocation），仅数据
// 来源不同：EvalSet json 替代扩展 HTTP payload。评估经 evalRunner 包装器把用例输入
// 映射为 InvocationInput 后走同一组装函数，保证评估路径与生产一致。
//
// 用法（项目根目录）：
//
//	go run ./cmd/absorb-eval                          # 默认跑 understanding-basic，3 次
//	go run ./cmd/absorb-eval -set knowledge-relevance # 单跑指定评估集
//	make eval SET=understanding-basic RUNS=3          # Makefile 入口
//
// 数据布局：<data>/<appName>/<evalSetId>.evalset.json 与同名 .metrics.json（appName
// 固定为生产 AppName，即 design/eval/absorb-reader/）。结果落盘 <out>/absorb-reader/。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/evaluation"
	"trpc.group/trpc-go/trpc-agent-go/evaluation/evalresult"
	evalresultlocal "trpc.group/trpc-go/trpc-agent-go/evaluation/evalresult/local"
	"trpc.group/trpc-go/trpc-agent-go/evaluation/evalset"
	evalsetlocal "trpc.group/trpc-go/trpc-agent-go/evaluation/evalset/local"
	"trpc.group/trpc-go/trpc-agent-go/evaluation/evaluator/registry"
	"trpc.group/trpc-go/trpc-agent-go/evaluation/metric"
	"trpc.group/trpc-go/trpc-agent-go/event"
	// metric/registry 是判据扩展注册表（rouge tokenizer 等），与 evaluator/registry（评分器注册表）不同
	metriclocal "trpc.group/trpc-go/trpc-agent-go/evaluation/metric/local"
	metricregistry "trpc.group/trpc-go/trpc-agent-go/evaluation/metric/registry"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/runner"

	"absorb-agent/internal/chat"
	"absorb-agent/internal/conf"
	"absorb-agent/internal/log"
	"absorb-agent/internal/store"
)

// evalRunner 生产 runner 包装器：把评估用例（userContent + sessionInput.State）经
// chat.PrepareInvocation 组装后再交给真实 runner。评估框架只认 runner.Runner 接口，
// 组装逻辑（scope 继承/参考材料/页面缓存/提示词/工具状态/工作集 L1）全部复用生产实现。
type evalRunner struct {
	inner runner.Runner
	sess  *store.SessionService
	ps    chat.PromptStrategy
	wsFn  func() conf.WorkingSetConfig
}

// Run 拦截评估框架的每次调用：userContent 即用户问题；sessionInput.State 经
// agent.WithRuntimeState 随 opts 传入（见 evaluation/service/internal/inference）。
func (w *evalRunner) Run(ctx context.Context, userID, sessionID string, message model.Message, opts ...agent.RunOption) (<-chan *event.Event, error) {
	ro := agent.NewRunOptions(opts...)
	in := chat.InvocationInput{Question: message.Content, SessionID: sessionID}
	if st := ro.RuntimeState; st != nil {
		in.Context = &chat.ContextInput{
			Text:          stateStr(st, "quote"),
			Paragraph:     stateStr(st, "paragraph"),
			PageContext:   stateStr(st, "page_context"),
			URL:           stateStr(st, "source_url"),
			PageMd:        stateStr(st, "page_md"),
			ContentStatus: stateStr(st, "content_status"),
		}
		if u := stateStr(st, "page_url"); u != "" || stateStr(st, "page_title") != "" {
			in.Page = &chat.PageInput{URL: u, Title: stateStr(st, "page_title")}
		}
	}
	prep, err := chat.PrepareInvocation(ctx, chat.AppName, userID, in, chat.InvocationDeps{
		SessionSvc: w.sess,
		PS:         w.ps,
		WSFn:       w.wsFn,
	})
	if err != nil {
		return nil, err
	}
	return w.inner.Run(prep.RunCtx, userID, prep.SessionID, prep.Message, append(opts, prep.RunOpts...)...)
}

// Close 透传释放生产 runner 资源（满足 runner.Runner 接口）。
func (w *evalRunner) Close() error { return w.inner.Close() }

// stateStr 读 SessionInput.State 中的字符串键（缺键/类型不符返回空串）。
func stateStr(st map[string]any, key string) string {
	if v, ok := st[key].(string); ok {
		return v
	}
	return ""
}

// cjkCharTokenizer 中文兼容的 rouge 分词器：框架内置分词器只认 ASCII 词
// （^[a-z0-9]+$），中文文本会被切成零 token 导致 rouge 恒 0。此实现按 CJK 字符
// 逐字切分（unigram），连续拉丁字母/数字按词切分，其余字符视作分隔。
// metrics.json 中以 "tokenizerName": "cjk-char" 引用。
type cjkCharTokenizer struct{}

func (cjkCharTokenizer) Tokenize(text string) []string {
	text = strings.ToLower(text)
	var tokens []string
	var word strings.Builder
	flush := func() {
		if word.Len() > 0 {
			tokens = append(tokens, word.String())
			word.Reset()
		}
	}
	for _, r := range text {
		switch {
		case unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r):
			flush()
			tokens = append(tokens, string(r))
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			word.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return tokens
}

func main() {
	cfgPath := flag.String("config", "config/config.yaml", "生产配置文件（模型/代理/抓取超时等沿用生产链路）")
	dataDir := flag.String("data", "design/eval", "评估数据根目录（<data>/absorb-reader/<set>.evalset.json）")
	outDir := flag.String("out", "design/eval/results", "评估结果输出目录")
	setID := flag.String("set", "understanding-basic", "要执行的评估集 ID")
	runs := flag.Int("runs", 3, "每个用例的重复运行次数")
	flag.Parse()

	if _, err := os.Stat(*cfgPath); err != nil {
		fmt.Fprintf(os.Stderr, "读取配置失败: %v（评估沿用生产 config.yaml，需先配置默认模型）\n", err)
		os.Exit(1)
	}
	cfg := conf.LoadConfig(*cfgPath)
	if err := log.InitLogging(cfg.Log); err != nil {
		fmt.Fprintf(os.Stderr, "init logging failed: %v\n", err)
		os.Exit(1)
	}

	// 临时隔离会话库：评估不读写生产 data/sessions.db，跑完即删
	tmpDir, err := os.MkdirTemp("", "absorb-eval-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "创建临时目录失败: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmpDir)
	dbPath, err := filepath.Abs(filepath.Join(tmpDir, "sessions.db"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "解析库路径失败: %v\n", err)
		os.Exit(1)
	}
	cfg.Store.Backend = "sqlite"
	cfg.Store.SQLite.Path = dbPath

	ctx := context.Background()
	sessSvc, err := store.New(cfg.Store.Backend, dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "创建临时会话库失败: %v\n", err)
		os.Exit(1)
	}
	defer sessSvc.Close()

	// 与生产 startCore 同一导入链：config.yaml 的模型配置进临时库作为默认模型
	migrated, err := sessSvc.MigrateLegacyModelConfig(ctx, chat.AppName,
		cfg.Model.Name, cfg.Model.APIKey, cfg.Model.BaseURL,
		cfg.Model.Temperature, cfg.Model.MaxTokens, cfg.Model.Thinking.Enabled)
	if err != nil {
		log.Warnf("[Absorb] migrate model config from config.yaml failed: %v", err)
	}
	if !migrated {
		fmt.Fprintln(os.Stderr, "评估中止：没有可用默认模型（config.yaml 未配置完整模型信息）")
		os.Exit(1)
	}
	if mc, _ := sessSvc.GetDefaultModelConfig(ctx, chat.AppName); mc != nil {
		log.Infof("[Absorb] eval default model: %s (%s)", mc.ModelID, mc.BaseURL)
	}

	// 画像注入与长期记忆关闭：M0 评测基线要求确定性（生产由门控热切换，此处静态关闭）
	gate := chat.NewMemoryGate(false)
	strategy, err := chat.NewPromptStrategy(cfg.Model.Variant, chat.WithProfileGate(gate))
	if err != nil {
		fmt.Fprintf(os.Stderr, "创建提示词策略失败: %v\n", err)
		os.Exit(1)
	}

	r, err := chat.NewRunner(cfg, func() string { return cfg.Proxy.URL }, strategy, sessSvc, gate)
	if err != nil {
		fmt.Fprintf(os.Stderr, "创建 runner 失败: %v\n", err)
		os.Exit(1)
	}
	defer r.Close()

	// 评估三件套（local 模式）：evalset/metric 读 <data>/<appName>/，结果写 <out>/<appName>/
	// metricRegistry 承载判据扩展（rouge tokenizer 等），随 metric 解析（Resolve）生效
	metricReg := metricregistry.New()
	// 注册中文兼容分词器（内置分词器对中文产生零 token），供 metrics.json 的
	// finalResponse.rouge.tokenizerName 引用
	if err := metricReg.RegisterRougeTokenizer("cjk-char", cjkCharTokenizer{}); err != nil {
		fmt.Fprintf(os.Stderr, "注册中文分词器失败: %v\n", err)
		os.Exit(1)
	}
	evalSetManager := evalsetlocal.New(evalset.WithBaseDir(*dataDir))
	metricManager := metriclocal.New(metric.WithBaseDir(*dataDir))
	evalResultManager := evalresultlocal.New(evalresult.WithBaseDir(*outDir))

	agentEvaluator, err := evaluation.New(chat.AppName, &evalRunner{
		inner: r,
		sess:  sessSvc,
		ps:    strategy,
		wsFn:  func() conf.WorkingSetConfig { return cfg.WorkingSet },
	},
		evaluation.WithEvalSetManager(evalSetManager),
		evaluation.WithMetricManager(metricManager),
		evaluation.WithEvalResultManager(evalResultManager),
		evaluation.WithRegistry(registry.New()),
		evaluation.WithMetricRegistry(metricReg),
		evaluation.WithNumRuns(*runs),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "创建评估器失败: %v\n", err)
		os.Exit(1)
	}
	defer agentEvaluator.Close()

	result, err := agentEvaluator.Evaluate(ctx, *setID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "评估执行失败: %v\n", err)
		os.Exit(1)
	}

	// 终端摘要（明细已由 evalresultlocal 落盘 outDir）
	fmt.Println("== Absorb M0 评估完成 ==")
	fmt.Printf("评估集: %s | 运行次数: %d | 总体: %s\n", result.EvalSetID, *runs, result.OverallStatus)
	failed := 0
	for _, caseResult := range result.EvalCases {
		fmt.Printf("用例 %-32s -> %s\n", caseResult.EvalCaseID, caseResult.OverallStatus)
		for _, mr := range caseResult.MetricResults {
			fmt.Printf("    %-28s score=%.2f threshold=%.2f -> %s\n",
				mr.MetricName, mr.Score, mr.Threshold, mr.EvalStatus)
		}
		if !strings.EqualFold(string(caseResult.OverallStatus), "passed") {
			failed++
		}
	}
	fmt.Printf("结果目录: %s\n", filepath.Join(*outDir, chat.AppName))
	if failed > 0 {
		fmt.Printf("未通过用例: %d/%d\n", failed, len(result.EvalCases))
		os.Exit(2)
	}
}
