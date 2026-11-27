// Package chat 问答编排层：扩展端 /api/ask 的完整链路（runner 调用、SSE 推流、
// 会话/scope/记忆/摘要的编排组装）。
//
// 一轮问答的流水线与文件对应：
//   - handler.go      SSE 端点与事件泵：流式转发、错误归一化、ask_user 挂起检测、取消
//   - resume.go       中断恢复：会话末悬挂 tool_calls 先补执行再答新问题（两阶段编排）
//   - invocation.go   runner 入参组装：scope 归组 → 参考材料重申 → 页面/文档缓存 → 工具状态 → 工作集 L1
//   - prompt.go       提示词策略：按模型族差异化 system/user message（prompts/ 编译期内嵌）
//   - workingset.go   工作集 L1 纯逻辑：跨文章连续性注入（不落库、逐 run）
//   - model.go        模型客户端工厂：DynamicModel 跟随默认模型配置 + 厂商方言 + 连通性探测
//   - memory.go       长期记忆服务装配与运行时门控（设置页热切换）
//   - summarizer.go   会话摘要装配（cache-safe fork，摘要卡消费侧包装）
//   - answer.go       ask_user 续答端点（官方 Human-in-the-Loop）
//   - capabilities.go 能力下发端点（扩展端视觉软闸数据源）
//   - dialect.go      服务商协议方言补丁（按变体登记）
//
// 术语：scope/filterKey=按文章哈希的会话隔离标识；L1/L2/L3=工作集/摘要/画像三层记忆；
// pendingAsk=ask_user 工具挂起态（本轮语义终点，续答走 /api/ask/answer）。
package chat

import (
	"fmt"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/agent/llmagent"
	"trpc.group/trpc-go/trpc-agent-go/memory"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/runner"
	"trpc.group/trpc-go/trpc-agent-go/tool"

	askuser "absorb-agent/tool/askuser"
	fetch "absorb-agent/tool/fetch_page"
	readpage "absorb-agent/tool/read_page"
	recall "absorb-agent/tool/recall_history"
	searchdoc "absorb-agent/tool/search_document"

	"absorb-agent/internal/conf"
	"absorb-agent/internal/log"
	"absorb-agent/internal/profile"
	"absorb-agent/internal/store"
)

const (
	// AppName 会话归属的应用标识（runner 与记忆库共用）。
	AppName   = "absorb-reader"
	agentName = "reading-assistant"
)

// NewRunner 构建一个带 LLMAgent 的 Runner：模型经 m 动态选择（由装配方创建的
// DynamicModel 注入，问答、摘要、画像蒸馏与知识草稿共用同一实例，跟随"默认模型"
// 切换即时生效）。会话存 SQLite（重启后历史不丢）。sessionSvc 由 main 启动时
// 创建并注入（同一实例也给 handler 做参考材料重申、给 DynamicModel 读默认模型）。
// gate 为记忆运行时开关（与画像注入/蒸馏共用），设置页热切换只翻转它。
func NewRunner(cfg conf.Config, proxyFn func() string, m model.Model, ps PromptStrategy, sessionSvc *store.SessionService, gate *MemoryGate) (runner.Runner, error) {
	if gate == nil {
		gate = NewMemoryGate(cfg.Memory.Enabled) // 防御：未注入门控时按配置静态装配
	}
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

	// 跨文章长会话摘要压缩：摘要器跟随默认模型（DynamicModel），阈值判定在 store 层
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
			readpage.NewReadPageTool(),        // 网页章节读取：目录 + 按节读全文（确定性定位，替代关键词猜词）
			searchdoc.NewSearchDocumentTool(), // 长文档（PDF）全文证据检索（M1：chunks+FTS5，30k 缓存之外可达）+ 读页模式
			fetchTool,                         // 需要访问 URL 资源时直接抓取（基于 httpfetch，开主内容提取）
			recall.NewRecallHistoryTool(),     // 摘要折叠细节按需回溯（index 目录 / fetch 原文切片）
			askuser.NewAskUserTool(),          // 指代无法消解/Guided 开场时向用户提问（LongRunner pending，提问即终止本轮）
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
