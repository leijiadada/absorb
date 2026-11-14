package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/runner"

	"absorb-agent/internal/bridge"
	"absorb-agent/internal/chat"
	"absorb-agent/internal/conf"
	"absorb-agent/internal/document"
	docextract "absorb-agent/internal/document/extract"
	"absorb-agent/internal/extsync"
	"absorb-agent/internal/knowledge"
	"absorb-agent/internal/log"
	"absorb-agent/internal/nativehost"
	"absorb-agent/internal/obs"
	"absorb-agent/internal/store"
	"absorb-agent/internal/store/storage"
	"absorb-agent/internal/telemetry/tokentracker"
	"absorb-agent/internal/upgrade"
	fetch_page "absorb-agent/tool/fetch_page"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

// 桌面端版本号与构建时间；默认值供源码运行，正式构建由 Makefile 经 ldflags -X 注入。
// /health 特征字段与"关于"页共用。
var (
	desktopVersion = "dev"
	builtAt        = ""
)

// versionDisplay 版本展示串：注入了构建时间则一并展示。
func versionDisplay() string {
	if builtAt == "" {
		return desktopVersion
	}
	return desktopVersion + " (" + builtAt + ")"
}

func main() {
	// 升级子命令预分流：必须在 flag.Parse 之前判断——否则 "upgrade" 会被
	// flag 当作 --cli 的位置参数，被子命令解析吞掉。
	if len(os.Args) > 1 && os.Args[1] == "upgrade" {
		os.Exit(runUpgradeCLI(os.Args[2:]))
	}

	cli := flag.Bool("cli", false, "以纯命令行服务模式运行（无桌面窗口，供调试/CI 使用）")
	nativeHost := flag.Bool("native-host", false, "以 Native Messaging 宿主模式运行（由浏览器拉起，无 GUI）")
	registerNH := flag.Bool("register-native-host", false, "生成 native-host.json 并写入 Edge/Chrome 注册表（HKCU）")
	unregisterNH := flag.Bool("unregister-native-host", false, "移除 Native Messaging 注册表项")
	flag.Parse()

	// 浏览器按 native-host.json 拉起宿主时不传 flag，首参即 origin
	// （chrome-extension://<id>/ ...）——识别为宿主模式，避免误启 GUI 被
	// 单实例锁弹窗并循环拉起。
	if args := flag.Args(); len(args) > 0 && strings.HasPrefix(args[0], "chrome-extension://") {
		nativehost.RunNativeHost(args)
		return
	}

	switch {
	case *nativeHost:
		nativehost.RunNativeHost(flag.Args())
		return
	case *registerNH:
		if err := nativehost.RegisterNativeHost(); err != nil {
			fmt.Fprintf(os.Stderr, "register failed: %v\n", err)
			os.Exit(1)
		}
		return
	case *unregisterNH:
		if err := nativehost.UnregisterNativeHost(); err != nil {
			fmt.Fprintf(os.Stderr, "unregister failed: %v\n", err)
			os.Exit(1)
		}
		return
	case *cli:
		runCLI()
		return
	}
	runDesktop()
}

// runCLI 原有命令行服务形态：加载配置 → 初始化 → 起前台 HTTP 服务。
// 桌面化后保留该模式，供无窗口环境调试与扩展链路回归测试。
func runCLI() {
	cfgPath := "config/config.yaml"
	if len(os.Args) > 2 {
		cfgPath = os.Args[2]
	}
	if abs, err := filepath.Abs(cfgPath); err == nil {
		cfgPath = abs
	}

	// 缺失则按内嵌默认生成配置/提示词，避免 LoadConfig 因文件不存在而 panic
	ensureDefaultConfig(cfgPath)

	cfg := conf.LoadConfig(cfgPath)

	// 先初始化文件日志（logs），此后所有记录落盘、终端不再打印
	if err := log.InitLogging(cfg.Log); err != nil {
		fmt.Fprintf(os.Stderr, "init logging failed: %v\n", err)
		os.Exit(1)
	}
	// 生产链路追踪（默认关闭；失败降级为告警，不阻断服务）
	if err := obs.Init(cfg.Telemetry, cfg.Log, telemetryOpts(cfg)...); err != nil {
		log.Warnf("[Absorb] telemetry init failed (tracing disabled): %v", err)
	}
	defer obs.Close()

	r, sessionSvc, strategy, _ := startCore(cfg, cfgPath, func() string { return cfg.Proxy.URL })
	defer sessionSvc.Close()
	defer r.Close()

	mux := http.NewServeMux()
	reg := chat.NewRunRegistry()
	// 知识持久化端点（M1）：与问答共用同一 SQLite 实例与归属维度
	ksvc := knowledge.NewService(sessionSvc.Storage())
	knowledge.NewHTTP(ksvc, chat.AppName, chat.UserID).
		SetDraft(chat.KnowledgeDraftFn(sessionSvc, func() string { return cfg.Proxy.URL })).Register(mux)
	// 扩展端会话镜像同步端点（恢复通道，与知识端点同归属维度）
	extsync.NewHTTP(sessionSvc.Storage(), chat.AppName, chat.UserID).Register(mux)
	// 文档准备端点（PDF 解读 PR2）：与问答同款代理/上限配置（CLI 调试形态静态读取）
	newDocumentAPI(sessionSvc, func() string { return cfg.Proxy.URL },
		func() int64 { return int64(cfg.Fetch.MaxContentMB) * 1024 * 1024 }).Register(mux)
	// 链路查看器（telemetry.sqlite 形态才有值）
	if th := obs.TraceHTTP(); th != nil {
		th.Register(mux)
	}
	// method-aware 模式与方法校验同 bridge 包保持一致
	mux.HandleFunc("POST /api/ask", chat.AskHandler(r, strategy, sessionSvc, chat.AppName, chat.UserID, reg,
		func() conf.WorkingSetConfig { return cfg.WorkingSet }, ksvc)) // CLI 调试形态：配置静态读取
	mux.HandleFunc("OPTIONS /api/ask", bridge.CorsPreflight)
	mux.HandleFunc("POST /api/cancel", chat.CancelHandler(r, reg))
	mux.HandleFunc("OPTIONS /api/cancel", bridge.CorsPreflight)
	// ask_user 提问的用户回答端点（官方 HITL：更新 JSON 作为新 user message 续答）
	mux.HandleFunc("POST /api/ask/answer", chat.AnswerHandler(r, strategy, sessionSvc, chat.AppName, chat.UserID, reg))
	mux.HandleFunc("OPTIONS /api/ask/answer", bridge.CorsPreflight)
	// 能力下发与 GUI 桥保持同链路（扩展端视觉软闸数据源；GUI 侧注册见 bridge.Deps.Capabilities）
	mux.HandleFunc("GET /api/capabilities", chat.CapabilitiesHandler(sessionSvc, chat.AppName))
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Write([]byte("ok"))
	})

	// 动态端口（127.0.0.1:0）：调试形态无固定端口需求，端口被占不再起不来。
	// 端口发现对齐桌面 bridge.port 机制：写数据目录下的 cli.port（独立命名，
	// 避免与桌面实例的 bridge.port 互相覆盖）；实际地址另打启动日志供人工查看。
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("listen failed: %v", err)
	}
	log.Infof("Absorb agent started: http://%s (config: %s) (log: %s)", l.Addr(), cfgPath, cfg.Log.Dir)
	portDir := filepath.Dir(cfg.Store.SQLite.Path)
	if portDir == "" || portDir == "." {
		portDir = filepath.Dir(cfg.Log.Dir)
	}
	portFile := filepath.Join(portDir, "cli.port")
	if werr := os.WriteFile(portFile, []byte(strconv.Itoa(l.Addr().(*net.TCPAddr).Port)), 0o644); werr != nil {
		log.Warnf("write cli port file failed: %v", werr)
	} else {
		defer os.Remove(portFile) // 正常退出清理；强杀残留由下次启动覆盖
	}
	if err := http.Serve(l, mux); err != nil {
		log.Fatalf("server exited: %v", err)
	}
}

// newDocumentAPI 文档准备端点（PDF 解读 PR2）：复用 fetch 公共获取服务，代理与
// 内容上限经闭包实时读取（与 fetch_page 同款热生效约定；CLI 形态传静态闭包）。
func newDocumentAPI(sessionSvc *store.SessionService, proxyFn func() string, maxBytesFn func() int64) *document.API {
	return document.NewAPI(document.NewService(
		sessionSvc.Storage(),
		docextract.NewPDFExtractor(fetch_page.NewFetchPageService(proxyFn, maxBytesFn)),
		chat.AppName, chat.UserID,
	))
}

// runDesktop 桌面形态：数据根解析 → 初始化（同 CLI）→ 内嵌 HTTP 桥 → Wails 窗口 + 托盘
func runDesktop() {
	dataRoot := resolveDataRoot()

	cfgPath := filepath.Join(dataRoot, "config", "config.yaml")
	ensureDefaultConfig(cfgPath)

	cfg := conf.LoadConfig(cfgPath)

	if err := log.InitLogging(cfg.Log); err != nil {
		fmt.Fprintf(os.Stderr, "init logging failed: %v\n", err)
		os.Exit(1)
	}
	// 生产链路追踪（默认关闭；失败降级为告警，不阻断服务）。Close 挂在
	// app.shutdown（wails.OnShutdown），此处兜底一份（幂等）。
	if err := obs.Init(cfg.Telemetry, cfg.Log, telemetryOpts(cfg)...); err != nil {
		log.Warnf("[Absorb] telemetry init failed (tracing disabled): %v", err)
	}
	defer obs.Close()

	// 配置运行时句柄：设置页保存后经句柄更新，请求侧实时读取（热切换）
	cfgH := conf.NewHandle(cfg)
	r, sessionSvc, strategy, memGate := startCore(cfg, cfgPath, func() string { return cfgH.Get().Proxy.URL })
	defer sessionSvc.Close()
	defer r.Close()

	// 扩展问答入口：内嵌 HTTP 桥（复用原 handler，问答唯一链路）。
	// 端口由 OS 动态分配（不使用 cfg.Server.Port），实际端口写入数据根的
	// bridge.port 供 Native 宿主与扩展发现；写失败仅告警，不影响服务运行。
	reg := chat.NewRunRegistry()
	// 知识持久化端点（M1）：与问答共用同一 SQLite 实例与归属维度，经 Extra 整组注册进桥
	ksvc := knowledge.NewService(sessionSvc.Storage())
	kapi := knowledge.NewHTTP(ksvc, chat.AppName, chat.UserID).
		SetDraft(chat.KnowledgeDraftFn(sessionSvc, func() string { return cfgH.Get().Proxy.URL }))
	// 文档准备端点（PDF 解读 PR2）：代理与内容上限经闭包实时读取（热生效，同 fetch_page）
	docAPI := newDocumentAPI(sessionSvc, func() string { return cfgH.Get().Proxy.URL },
		func() int64 { return int64(cfgH.Get().Fetch.MaxContentMB) * 1024 * 1024 })
	br := bridge.New(0, versionDisplay(), bridge.Deps{
		Ask:          chat.AskHandler(r, strategy, sessionSvc, chat.AppName, chat.UserID, reg, func() conf.WorkingSetConfig { return cfgH.Get().WorkingSet }, ksvc),
		Cancel:       chat.CancelHandler(r, reg),
		Capabilities: chat.CapabilitiesHandler(sessionSvc, chat.AppName),
		Extra: func(mux *http.ServeMux) {
			kapi.Register(mux)
			docAPI.Register(mux)
			extsync.NewHTTP(sessionSvc.Storage(), chat.AppName, chat.UserID).Register(mux)
			// ask_user 提问的用户回答端点（官方 HITL 续答）
			mux.HandleFunc("POST /api/ask/answer", chat.AnswerHandler(r, strategy, sessionSvc, chat.AppName, chat.UserID, reg))
			mux.HandleFunc("OPTIONS /api/ask/answer", bridge.CorsPreflight)
			if th := obs.TraceHTTP(); th != nil { // 链路查看器（telemetry.sqlite 形态才有值）
				th.Register(mux)
			}
		},
	})
	br.Start()
	if err := br.StartErr(); err != nil {
		log.Errorf("HTTP bridge failed to start (extension cannot connect): %v", err)
	} else if werr := os.WriteFile(filepath.Join(dataRoot, "bridge.port"),
		[]byte(strconv.Itoa(br.Port())), 0o644); werr != nil {
		log.Warnf("write bridge port file failed (extension cannot discover port automatically): %v", werr)
	}

	app := NewApp(cfgH, cfgPath, dataRoot, r, strategy, sessionSvc, reg, br, memGate, nil)

	// 升级服务（桌面形态）：Notifier 经 Wails 事件推前端，PreApplyHook 做升级前备份。
	// 装配发生在 App 创建后（Notifier 需要 app.ctx），配置关闭时保持 nil。
	if cfg.Upgrade.On() {
		app.upg = newUpgradeService(cfg, upgradeHost{
			version: desktopVersion,
			builtAt: builtAt,
			channel: cfg.Upgrade.Channel,
			dataDir: dataRoot,
		},
			upgrade.WithNotifier(updateNotifier{app: app}),
			upgrade.WithPreApply(storeMaintainer{svc: sessionSvc}),
		)
	}

	err := wails.Run(&options.App{
		Title:             "Absorb 阅读助手",
		Width:             960,
		Height:            680,
		MinWidth:          800,
		MinHeight:         560,
		Frameless:         true, // 无边框：品牌蓝顶栏兼做标题栏（拖拽/双击最大化/窗口控制按钮在前端），对齐 Docker Desktop
		AssetServer:       &assetserver.Options{Assets: frontendAssets},
		BackgroundColour:  &options.RGBA{R: 247, G: 248, B: 250, A: 255},
		OnStartup:         app.startup,
		HideWindowOnClose: true, // 关窗 = 隐藏到托盘，服务继续跑
		OnShutdown:        app.shutdown,
		Bind:              []interface{}{app},
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId:               "absorb-agent-desktop-8f4a2c",
			OnSecondInstanceLaunch: app.onSecondInstance,
		},
		Windows: &windows.Options{
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
		},
	})
	if err != nil {
		log.Fatalf("desktop app exited: %v", err)
	}
}

// telemetryOpts sqlite 查看器形态选项：telemetry.sqlite 开启时把 span 落会话库的
// request_traces 表并开放 /debug/traces（endpoint 非空走 OTLP 时被忽略）。
// obs.Init 先于 startCore 建库执行，exporter 打开连接时幂等兜底建表。
func telemetryOpts(cfg conf.Config) []obs.Option {
	if !cfg.Telemetry.SQLite {
		return nil
	}
	dbPath, err := filepath.Abs(cfg.Store.SQLite.Path)
	if err != nil {
		return nil
	}
	return []obs.Option{obs.WithSQLiteStore(dbPath)}
}

// startCore 初始化日志之外的公共核心：SQLite 会话库、提示词策略、Runner（CLI/桌面共用）。
// proxyFn 返回当前代理地址，设置保存后经其即时生效（桌面端闭包捕获配置句柄）。
// 返回记忆运行时开关（与画像注入/蒸馏共用），桌面端设置页热切换记忆时翻转它。
func startCore(cfg conf.Config, cfgPath string, proxyFn func() string) (runner.Runner, *store.SessionService, chat.PromptStrategy, *chat.MemoryGate) {
	dbPath, err := filepath.Abs(cfg.Store.SQLite.Path)
	if err != nil {
		log.Fatalf("resolve db path failed: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		log.Fatalf("create db dir failed: %v", err)
	}
	log.Infof("SQLite path: %s", dbPath)

	sessionSvc, err := store.New(cfg.Store.Backend, dbPath)
	if err != nil {
		log.Fatalf("create storage backend %q failed: %v", cfg.Store.Backend, err)
	}

	// token 用量统计（telemetry 第一批）：内存聚合 + SQLite 流水异步落库（旁路：
	// sink 在独立 goroutine 中执行，失败仅告警，绝不影响问答主链路）。
	tokentracker.SetSink(func(rec tokentracker.Record) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := sessionSvc.InsertTokenUsage(ctx, storage.TokenUsageRecord{
			TS:               rec.TS,
			Kind:             rec.Kind,
			Model:            rec.Model,
			PromptTokens:     rec.PromptTokens,
			CompletionTokens: rec.CompletionTokens,
			ReasoningTokens:  rec.ReasoningTokens,
			TotalTokens:      rec.TotalTokens,
			CachedTokens:     rec.CachedTokens,
		}); err != nil {
			log.Warnf("[Absorb] token usage persist failed: %v", err)
		}
	})
	log.Infof("[Absorb] token tracker ready (memory agg + sqlite sink)")

	// 首启迁移：模型表为空且 config.yaml 有完整模型配置时导入为第一条默认
	migrated, err := sessionSvc.MigrateLegacyModelConfig(context.Background(), chat.AppName,
		cfg.Model.Name, cfg.Model.APIKey, cfg.Model.BaseURL,
		cfg.Model.Temperature, cfg.Model.MaxTokens, cfg.Model.Thinking.Enabled)
	if err != nil {
		log.Warnf("[Absorb] migrate model config from config.yaml failed: %v", err)
	} else if migrated {
		log.Infof("[Absorb] migrated default model from config.yaml: %s", cfg.Model.Name)
	}
	logDefaultModel(sessionSvc)

	// 记忆运行时开关：初始值取配置，设置页保存后热切换（服务常驻，仅翻转开关）
	gate := chat.NewMemoryGate(cfg.Memory.Enabled)

	strategy, err := chat.NewPromptStrategy(cfg.Model.Variant,
		chat.WithProfilePath(cfg.Memory.Profile.FilePath), chat.WithProfileGate(gate))
	if err != nil {
		log.Fatalf("create prompt strategy failed: %v", err)
	}
	log.Infof("prompt strategy: %s (%s)", strategy.ID(), chat.PromptDigest())

	if cfg.Skills.Root != "" {
		if abs, err := filepath.Abs(cfg.Skills.Root); err == nil {
			cfg.Skills.Root = abs
		}
		log.Infof("Skills root: %s", cfg.Skills.Root)
	}

	r, err := chat.NewRunner(cfg, proxyFn, strategy, sessionSvc, gate)
	if err != nil {
		log.Fatalf("create runner failed: %v", err)
	}
	return r, sessionSvc, strategy, gate
}

// logDefaultModel 记录当前默认模型；未配置时告警（问答将返回引导错误）。
func logDefaultModel(sessionSvc *store.SessionService) {
	mc, err := sessionSvc.GetDefaultModelConfig(context.Background(), chat.AppName)
	switch {
	case err != nil:
		log.Warnf("[Absorb] read default model failed: %v", err)
	case mc == nil:
		log.Warnf("[Absorb] no model configured; open the desktop app 'Model' page to add one")
	default:
		log.Infof("[Absorb] default model: %s (%s | %s)", displayModelName(mc), mc.ModelID, mc.BaseURL)
	}
}
