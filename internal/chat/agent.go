package chat

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // 记忆库驱动（internal/store 已注册同名驱动，此处显式声明依赖）
	"trpc.group/trpc-go/trpc-agent-go/agent/llmagent"
	"trpc.group/trpc-go/trpc-agent-go/memory"
	"trpc.group/trpc-go/trpc-agent-go/memory/extractor"
	memorystore "trpc.group/trpc-go/trpc-agent-go/memory/sqlite"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/model/openai"
	"trpc.group/trpc-go/trpc-agent-go/runner"
	"trpc.group/trpc-go/trpc-agent-go/session"
	"trpc.group/trpc-go/trpc-agent-go/tool"

	expand "absorb-agent/tool/expand_context"
	fetch "absorb-agent/tool/fetch_page"
	recall "absorb-agent/tool/recall_history"
	askuser "absorb-agent/tool/askuser"

	"absorb-agent/internal/conf"
	"absorb-agent/internal/errs"
	"absorb-agent/internal/kind"
	"absorb-agent/internal/log"
	"absorb-agent/internal/profile"
	"absorb-agent/internal/store"
)

const (
	// AppName 会话归属的应用标识（runner 与记忆库共用）。
	AppName   = "absorb-reader"
	agentName = "reading-assistant"
)

// errNoModel 尚未配置任何模型时的统一错误（问答/蒸馏均会收到）。
var errNoModel = errors.New("还没有可用模型，请先在桌面端“模型”页添加并设为默认")

// providerVariant 查服务商对应的 openai 协议变体（空=由框架按 base_url 自动推断）。
// 映射集中维护在 kind.Provider.Variant，避免散落多份真相源。
// 变体影响思考字段格式等厂商差异（如 GLM 的嵌套 thinking 结构）。
func providerVariant(provider string) string {
	return string(kind.Provider(provider).Variant())
}

// effectiveBaseURL 计算传给 openai 客户端的 base URL：
// 完整 URL 模式剥掉尾部 /chat/completions（客户端会自动补回），统一去尾斜杠。
func effectiveBaseURL(mc *store.ModelConfig) string {
	base := strings.TrimRight(strings.TrimSpace(mc.BaseURL), "/")
	if mc.FullURL {
		base = strings.TrimSuffix(base, "/chat/completions")
	}
	return base
}

// buildOpenAIModel 按模型配置构建 openai 兼容客户端。
// proxyFn 每次请求实时返回全局代理地址（空=跟随系统代理），与 fetch 共用同一解析逻辑，
// 保证配置保存后模型 API 调用也即时走代理，无需重启。
func buildOpenAIModel(mc *store.ModelConfig, proxyFn func() string) model.Model {
	opts := []openai.Option{
		openai.WithBaseURL(effectiveBaseURL(mc)),
		openai.WithAPIKey(mc.APIKey),
		// 按模型视觉能力决定是否强制字符串 content：ImageSupport=false 时剥离图片 part。
		// GLM 默认被标为支持图片（元数据未修正），此处放行，由端点返回 1210 报错。
		openai.WithTextOnlyMessageContent(!mc.ImageSupport),
		openai.WithChatRequestJSONCallback(func(ctx context.Context, body []byte, err error) {
			if err != nil {
				log.Errorf("[Absorb] request serialization error: %v", err)
				return
			}
			log.Debugf("[Absorb] request body sent to model:\n%s", string(body))
		}),
	}
	v := providerVariant(mc.Provider)
	if v != "" {
		opts = append(opts, openai.WithVariant(openai.Variant(v)))
		opts = append(opts, variantOptions(kind.Variant(v))...)
	}
	if proxyFn != nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.Proxy = fetch.ProxyResolver(proxyFn)
		opts = append(opts, openai.WithHTTPClientOptions(openai.WithHTTPClientTransport(tr)))
	}
	return openai.New(mc.ModelID, opts...)
}

// dynamicModel 按默认模型配置动态选择底层模型的包装器：
// 每次调用读取 default_model_id → 缓存命中直接复用，未命中构建并缓存；
// cacheKey 覆盖 model_id/base_url/key/变体，切换默认或编辑配置自动走新客户端，无需重启。
type dynamicModel struct {
	sess    *store.SessionService
	proxyFn func() string
	mu      sync.Mutex
	cache   map[string]model.Model
}

func newDynamicModel(sess *store.SessionService, proxyFn func() string) *dynamicModel {
	return &dynamicModel{sess: sess, proxyFn: proxyFn, cache: map[string]model.Model{}}
}

func (d *dynamicModel) Info() model.Info {
	m, _, err := d.current(context.Background())
	if err != nil || m == nil {
		return model.Info{Name: ""}
	}
	return m.Info()
}

func (d *dynamicModel) GenerateContent(ctx context.Context, req *model.Request) (<-chan *model.Response, error) {
	m, mc, err := d.current(ctx)
	if err != nil {
		return nil, err
	}
	applyModelGenConfig(req, mc)
	return m.GenerateContent(ctx, req)
}

func (d *dynamicModel) current(ctx context.Context) (model.Model, *store.ModelConfig, error) {
	mc, err := d.sess.GetDefaultModelConfig(ctx, AppName)
	if err != nil {
		return nil, nil, fmt.Errorf("读取默认模型配置失败: %w", err)
	}
	if mc == nil {
		return nil, nil, errNoModel
	}
	key := mc.ModelID + "|" + mc.APIKey + "|" + providerVariant(mc.Provider) + "|" + effectiveBaseURL(mc) + "|" + strconv.FormatBool(mc.ImageSupport)
	d.mu.Lock()
	defer d.mu.Unlock()
	if m, ok := d.cache[key]; ok {
		return m, mc, nil
	}
	m := buildOpenAIModel(mc, d.proxyFn)
	d.cache[key] = m
	return m, mc, nil
}

// applyModelGenConfig 把 per-model 生成参数覆盖到请求上；
// nil 值清空对应字段（跟随模型侧默认）。思考模式三态：
// 0=跟随默认（清空 ThinkingEnabled，不向请求下发思考参数）、1=开启、2=关闭。
func applyModelGenConfig(req *model.Request, mc *store.ModelConfig) {
	if req == nil {
		return
	}
	req.Temperature = mc.Temperature
	req.MaxTokens = mc.MaxTokens
	switch mc.Thinking {
	case 1:
		on := true
		req.ThinkingEnabled = &on
	case 2:
		off := false
		req.ThinkingEnabled = &off
	default:
		req.ThinkingEnabled = nil
	}
	req.ThinkingTokens = nil
}

// TestResult 连通性测试结果（App 绑定层返回给前端）。
type TestResult struct {
	OK        bool   `json:"ok"`
	LatencyMs int64  `json:"latencyMs"`
	Error     string `json:"error"`
}

// TestModelConn 用表单当前值发一次真实请求（max_tokens=1、思考关闭、非流式），
// 返回耗时或人话错误。proxyFn 复用全局代理解析（同问答链路）。
func TestModelConn(mc *store.ModelConfig, proxyFn func() string) TestResult {
	m := buildOpenAIModel(mc, proxyFn)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	mcCtx := errs.ModelContext{Provider: mc.Provider, ModelID: mc.ModelID, DisplayName: mc.DisplayName}
	thinkingOff := false
	maxTokens := 1
	req := &model.Request{
		Messages: []model.Message{{Role: model.Role("user"), Content: "ping"}},
		GenerationConfig: model.GenerationConfig{
			Stream:          false,
			MaxTokens:       &maxTokens,
			ThinkingEnabled: &thinkingOff,
		},
	}
	start := time.Now()
	ch, err := m.GenerateContent(ctx, req)
	if err != nil {
		return TestResult{Error: errs.FromError(err, mcCtx).Message}
	}
	done := false
	for resp := range ch {
		if resp == nil {
			continue
		}
		if resp.Error != nil {
			return TestResult{LatencyMs: elapsedMs(start), Error: errs.FromModel(resp.Error, mcCtx).Message}
		}
		if resp.Done {
			done = true
			break
		}
	}
	// 通道关闭仍未见到 Done：ctx 超时或异常中断，不能当成功返回
	if !done {
		if ctx.Err() != nil {
			return TestResult{LatencyMs: elapsedMs(start), Error: "连接超时，请检查网络或代理后重试"}
		}
		return TestResult{LatencyMs: elapsedMs(start), Error: "服务未返回完整响应"}
	}
	return TestResult{OK: true, LatencyMs: elapsedMs(start)}
}

func elapsedMs(start time.Time) int64 {
	return time.Since(start).Round(time.Millisecond).Milliseconds()
}

// NewRunner 构建一个带 LLMAgent 的 Runner：模型按默认模型配置动态选择，
// 会话存 SQLite（重启后历史不丢）。sessionSvc 由 main 启动时创建并注入
// （同一实例也给 handler 做参考材料重申、给 dynamicModel 读默认模型）。
// gate 为记忆运行时开关（与画像注入/蒸馏共用），设置页热切换只翻转它。
func NewRunner(cfg conf.Config, proxyFn func() string, ps PromptStrategy, sessionSvc *store.SessionService, gate *MemoryGate) (runner.Runner, error) {
	if gate == nil {
		gate = NewMemoryGate(cfg.Memory.Enabled) // 防御：未注入门控时按配置静态装配
	}
	// 问答与记忆蒸馏共用该包装模型：跟随"默认模型"切换，即时生效
	m := newDynamicModel(sessionSvc, proxyFn)
	genConfig := model.GenerationConfig{Stream: true} // 温度/输出上限/思考由 per-model 配置在请求层注入

	// Agent Skills：按配置装配技能仓库（仅知识加载）。
	// root 为空/目录不存在时返回空 options，技能不启用。
	skillsOpts, warns, err := conf.NewSkillsOptions(cfg.Skills.Root)
	if err != nil {
		return nil, fmt.Errorf("配置技能失败: %w", err)
	}
	for _, w := range warns {
		log.Warnf("[Absorb] %s", w)
	}

	// 长期记忆：服务常驻装配（enabled 由门控控制，热切换），初始化失败仅告警降级
	// （与 skills 装配同策略）。
	memSvc, memWarn := newMemoryService(cfg, m, gate)
	if memWarn != "" {
		log.Warnf("[Absorb] %s", memWarn)
	}
	if memSvc != nil {
		// 相关记忆改由提示词侧尾部注入（<related_memories>，当前轮 user 消息 = 前缀缓存
		// 安全区），替代框架 WithPreloadMemory 的 system 段预注入；检索经门控包装自动生效。
		if sp, ok := ps.(interface{ setMemoryReader(memory.Reader) }); ok {
			sp.setMemoryReader(memSvc)
		}
	}

	// fetch_page：代理地址、超时、内容上限均经 fn 每次请求实时读取（设置保存即时生效）
	fetchTimeoutFn := func() time.Duration { return time.Duration(cfg.Fetch.TimeoutSeconds) * time.Second }
	fetchMaxBytesFn := func() int64 { return int64(cfg.Fetch.MaxContentMB) * 1024 * 1024 }
	fetchTool, ferr := fetch.NewFetchPageTool(proxyFn, fetchTimeoutFn, fetchMaxBytesFn)
	if ferr != nil {
		return nil, fmt.Errorf("构建 fetch_page 工具失败: %w", ferr)
	}

	// 跨文章长会话摘要压缩：摘要器跟随默认模型（dynamicModel），阈值判定在 store 层
	// worker（scope 感知）；summary.disabled=true 关闭。
	if !cfg.Summary.Bool() {
		sessionSvc.SetSummarizer(newSessionSummarizer(m), store.SummaryRunnerOptions{
			EventThreshold:     cfg.Summary.EventThreshold,
			TokenThreshold:     cfg.Summary.TokenThreshold,
			QueueSize:          cfg.Summary.QueueSize,
			CascadeFullSession: false, // 跨分支全文级联：多一次 LLM 调用，默认关；单 scope 拷贝优化无条件生效
		})
		log.Infof("[Absorb] session summarizer enabled (event>%d || token>%d, queue=%d)",
			cfg.Summary.EventThreshold, cfg.Summary.TokenThreshold, cfg.Summary.QueueSize)
	}

	opts := []llmagent.Option{
		llmagent.WithModel(m),
		llmagent.WithDescription("帮助用户理解选中句子/段落的阅读助手"),
		llmagent.WithInstruction(ps.SystemInstruction()),
		llmagent.WithGenerationConfig(genConfig),
		// 摘要消费开关：SDK 默认 false，不打开则 ContentRequestProcessor 既不注入
		// scope 摘要、也不应用摘要 cutoff，摘要只有生产没有消费。无摘要时该开关为
		// no-op（cutoff 零值 → 全量重放），故无条件启用。
		llmagent.WithAddSessionSummary(true),
		// 摘要卡消费侧包装：hint 不持久化（store 层产纯摘要），注入模型请求时在此
		// 拼接，告知模型"细节已折叠，可经 recall_history 工具回溯"。
		llmagent.WithSummaryFormatter(summaryCardFormatter),
		// 摘要注入位置：默认 System 模式会把摘要卡拼进 system 消息，摘要每次重生成
		// （event>16 || token>24K 触发）都重写 system 段，连带炸掉跨会话共享的
		// system+工具前缀缓存。User 模式并入最老 user 消息（system 之后），触发时
		// 只重建历史部分，system 段从此逐字节稳定。
		llmagent.WithSessionSummaryInjectionMode(llmagent.SessionSummaryInjectionUser),
		llmagent.WithTools([]tool.Tool{
			expand.NewExpandContextTool(), // 当材料不足时反向请求浏览器扩充邻近段落
			fetchTool,                     // 需要访问 URL 资源时直接抓取（基于 httpfetch，开主内容提取）
			recall.NewRecallHistoryTool(), // 摘要折叠细节按需回溯（index 目录 / fetch 原文切片）
			askuser.NewAskUserTool(),      // 指代无法消解/Guided 开场时向用户提问（LongRunner pending，提问即终止本轮）
		}),
	}
	opts = append(opts, skillsOpts...)
	agt := llmagent.New(agentName, opts...)

	if memSvc != nil && cfg.Memory.Profile.Enabled {
		// L3 用户画像蒸馏：后台 ticker 按水位把 L1 原子记忆蒸馏为 profile.md；
		// 记忆总开关关闭时跳过（热切换由门控控制，ticker 常驻）
		profile.StartProfileDistiller(memSvc, cfg, m, memory.UserKey{AppName: AppName, UserID: UserID}, gate.Enabled)
		log.Infof("[Absorb] profile distiller started (interval %d min, watermark %d)",
			cfg.Memory.Profile.IntervalMinutes, cfg.Memory.Profile.MinNewEntries)
	}

	runnerOpts := []runner.Option{
		runner.WithSessionService(sessionSvc),
		// 中断落账（P0）：取消/超时打断流式输出时，把已流出的助手文本聚合成一条
		// 完整事件写入 session——摘要/参考材料重申/scope 记账从此能看到被中断的轮次。
		// 工具阶段被打断产生的悬挂 tool_calls 由恢复轮 WithResume 补执行（见 resume.go）。
		runner.WithPersistInterruptedAssistant(true),
	}
	if memSvc != nil {
		// 注册后 runner 会在每次响应结束后自动触发记忆提取（EnqueueAutoMemoryJob）
		runnerOpts = append(runnerOpts, runner.WithMemoryService(memSvc))
	}
	return runner.NewRunner(AppName, agt, runnerOpts...), nil
}

// MemoryGate 记忆功能运行时总开关：记忆服务、画像注入、画像蒸馏三方共用同一实例。
// 设置页保存时翻转此开关即热切换（无需重启）：结构资源（DB 连接、worker）常驻，
// 仅行为短路。新增"开关类"配置照此模式预留门控点即可。
type MemoryGate struct {
	on atomic.Bool
}

func NewMemoryGate(initial bool) *MemoryGate {
	g := &MemoryGate{}
	g.on.Store(initial)
	return g
}

func (g *MemoryGate) Enabled() bool { return g.on.Load() }

// Set 翻转开关（保存落盘成功后由设置层调用）。
func (g *MemoryGate) Set(on bool) { g.on.Store(on) }

// gatedMemoryService 记忆服务门控包装：关闭态短路读写与提取，开启态行为与底层服务完全一致。
// 内嵌 memory.Service 接口透传未覆写方法（Tools/Close 等）。
type gatedMemoryService struct {
	memory.Service
	gate *MemoryGate
}

func (s *gatedMemoryService) ReadMemories(ctx context.Context, userKey memory.UserKey, limit int) ([]*memory.Entry, error) {
	if !s.gate.Enabled() {
		return nil, nil
	}
	return s.Service.ReadMemories(ctx, userKey, limit)
}

func (s *gatedMemoryService) SearchMemories(ctx context.Context, userKey memory.UserKey, query string, opts ...memory.SearchOption) ([]*memory.Entry, error) {
	if !s.gate.Enabled() {
		return nil, nil
	}
	return s.Service.SearchMemories(ctx, userKey, query, opts...)
}

func (s *gatedMemoryService) AddMemory(ctx context.Context, userKey memory.UserKey, mem string, topics []string, opts ...memory.AddOption) error {
	if !s.gate.Enabled() {
		return nil
	}
	return s.Service.AddMemory(ctx, userKey, mem, topics, opts...)
}

func (s *gatedMemoryService) UpdateMemory(ctx context.Context, memoryKey memory.Key, mem string, topics []string, opts ...memory.UpdateOption) error {
	if !s.gate.Enabled() {
		return nil
	}
	return s.Service.UpdateMemory(ctx, memoryKey, mem, topics, opts...)
}

func (s *gatedMemoryService) DeleteMemory(ctx context.Context, memoryKey memory.Key) error {
	if !s.gate.Enabled() {
		return nil
	}
	return s.Service.DeleteMemory(ctx, memoryKey)
}

func (s *gatedMemoryService) ClearMemories(ctx context.Context, userKey memory.UserKey) error {
	if !s.gate.Enabled() {
		return nil
	}
	return s.Service.ClearMemories(ctx, userKey)
}

func (s *gatedMemoryService) EnqueueAutoMemoryJob(ctx context.Context, sess *session.Session) error {
	if !s.gate.Enabled() {
		return nil
	}
	return s.Service.EnqueueAutoMemoryJob(ctx, sess)
}

// newMemoryService 构建长期记忆服务（常驻）：SQLite 后端（独立子模块，replace 引入）+ LLM 提取器（auto 模式），
// 外层包门控——enabled 是运行时开关（热切换），不再决定服务是否装配。
// 返回 (nil, warn) 表示初始化失败，调用方降级为无记忆运行。
func newMemoryService(cfg conf.Config, m model.Model, gate *MemoryGate) (memory.Service, string) {
	db, err := sql.Open("sqlite",
		cfg.Memory.DBPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, "打开记忆库失败，记忆功能停用: " + err.Error()
	}
	svcOpts := []memorystore.ServiceOpt{
		// auto 模式：runner 响应结束后由框架异步提取事实写入记忆库
		memorystore.WithExtractor(extractor.NewExtractor(m)),
		// 记忆提取含 LLM 调用，耗时高于默认 30s
		memorystore.WithMemoryJobTimeout(2 * time.Minute),
		memorystore.WithAsyncMemoryNum(cfg.Memory.AsyncNum),
		memorystore.WithMemoryQueueSize(cfg.Memory.QueueSize),
		memorystore.WithSoftDelete(true), // 删除走墓碑
	}
	svc, err := memorystore.NewService(db, svcOpts...)
	if err != nil {
		_ = db.Close()
		return nil, "初始化记忆服务失败，记忆功能停用: " + err.Error()
	}
	return &gatedMemoryService{Service: svc, gate: gate}, ""
}
