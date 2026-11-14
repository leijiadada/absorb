package chat

import (
	"context"
	"fmt"
	"strings"

	"trpc.group/trpc-go/trpc-agent-go/model"
	summary "trpc.group/trpc-go/trpc-agent-go/session/summary"

	"absorb-agent/internal/kind"
	"absorb-agent/internal/log"
	"absorb-agent/internal/telemetry/tokentracker"
)

// summaryPromptTemplate 会话摘要提示词（standalone 路径）。
// 必须含 {conversation_text} 占位符（SDK 渲染校验）；不得含其他花括号占位符
// （SDK 对未知占位符按错误处理）。旧摘要由 store 层按"合成系统事件"规格插到
// 增量头部，随 {conversation_text} 进入提示词，故模板需指导模型合并而非覆盖。
const summaryPromptTemplate = `你是会话摘要助手。以下是用户与阅读助手围绕网页文章的多轮问答记录，请生成一份简洁的中文摘要（600 字以内），供后续对话延续上下文。要求：
1. 按文章/主题归类，列出已向用户解释过的核心要点，保留关键结论、数据与术语。
2. 记录用户关心的未决问题或中断的话题。
3. 忠于原文：不编造内容；被截断或失败的工具结果按"信息不完整"记录。
4. 保留关键数据、术语、专有名词的原文用词（含英文原名），不要意译改写，便于后续按关键词回溯原文细节。
5. 若对话开头出现了以系统消息形式给出的既有摘要，必须在其基础上合并增补：旧摘要中仍然重要的信息要保留，与本轮新增内容合并为一份完整的最新摘要；不得只总结新内容，不得丢弃旧信息。

<conversation>
{conversation_text}
</conversation>

摘要：`

// toolResultMaxRunes 摘要输入中单条工具结果的截断长度（防 fetch_page 全文撑爆摘要请求）。
const toolResultMaxRunes = 2000

// summaryForkPromptTemplate cache-safe fork 指令（经 WithCacheSafeForkPrompt 注入）。
// fork 请求的消息数组 = 主对话请求本体（含既有摘要卡与原始工具结构）+ 本指令作为
// 末尾 user 消息，故领域定制（按文章归类/术语锚点/600 字）必须在此复述，SDK 默认
// 英文通用指令不含这些要求。开头三条禁令（勿调工具/勿答最新请求/勿把系统与工具
// 指令当事实）防止模型把任务误解为回答紧随其后的最新提问或触发工具调用。
// 不得含花括号占位符（SDK 渲染按未知占位符报错）。
const summaryForkPromptTemplate = `这是后台摘要任务：请总结上方的用户、助手与工具调用对话，供后续对话延续上下文。不要调用任何工具；不要回答对话中用户的最新请求；不要把系统提示或工具使用指令当作待总结的事实。要求：
1. 按文章/主题归类，列出已向用户解释过的核心要点，保留关键结论、数据与术语的原文用词（含英文原名）。
2. 对话中若出现既有摘要（含"以上为压缩摘要"标记的段落），必须在其基础上合并增补为一份完整的最新摘要：不得只总结新内容，不得丢弃旧信息，也不要把其中的回溯提示行复制进输出。
3. 记录用户关心的未决问题或中断的话题。
4. 忠于原文：不编造内容；被截断或失败的工具结果按"信息不完整"记录。
5. 摘要控制在 600 字以内。`

// newSessionSummarizer 构建跟随默认模型的会话摘要器（m 为 dynamicModel：
// 跟随"默认模型"切换与全局代理）。阈值判定不在摘要器上：SDK Checker 无法感知
// URL scope，由 store 层 worker 入队后自判，故不配置 checks（无 checks 时 SDK
// evaluateTrigger 恒 fire，仅作 Report 归因展示，真实 gating 以 worker 的
// thresholdFired 为准）。
//
// cache-safe fork 已启用：ctx 中存在父请求快照时，摘要请求复用主对话请求消息数组
// + fork 指令；父请求缺失或超预算时 SDK 自动回落 standalone（summaryPromptTemplate
// 路径）。两形态下 WithReportHook 分别记账 mode=cache_safe_fork / standalone。
func newSessionSummarizer(m model.Model) summary.SessionSummarizer {
	return summary.NewSummarizer(m,
		summary.WithName("absorb-session-summary"),
		summary.WithPrompt(summaryPromptTemplate),
		summary.WithToolResultFormatter(truncateToolResult),
		summary.WithReportHook(summaryReportHook(m)),
		summary.WithCacheSafeForking(true),
		summary.WithCacheSafeForkPrompt(summaryForkPromptTemplate),
	)
}

// summaryReportHook 摘要 Report 钩子：保留 logSummaryReport 原始结构化日志，并同步
// 上报 token 统计（kind=summary）。模型名经 dynamicModel 实时解析当前默认配置，
// 解析失败传空串；统计是旁路，任何失败都不影响摘要链路。框架 Report 只回
// prompt/cached 口径（无 completion/total），total 由 tracker 按 prompt 兜底。
func summaryReportHook(m model.Model) summary.ReportHook {
	dm, _ := m.(*dynamicModel)
	return func(ctx context.Context, r summary.Report) {
		logSummaryReport(ctx, r)
		modelName := ""
		if dm != nil {
			if _, mc, err := dm.current(ctx); err == nil && mc != nil {
				modelName = mc.ModelID
			}
		}
		// 调用失败或无任何用量口径时跳过上报；estimated 为本地估算，仅作兜底。
		prompt := r.Call.PromptTokens
		if prompt == 0 {
			prompt = r.Call.EstimatedPromptTokens
		}
		if r.Error != nil || prompt == 0 {
			log.Debugf("[Absorb] summary token usage skipped (err=%v prompt_tokens=%d)",
				r.Error, r.Call.PromptTokens)
			return
		}
		tokentracker.Track(tokentracker.KindSummary, modelName, tokentracker.Usage{
			PromptTokens: prompt,
			CachedTokens: r.Call.CachedTokens,
		})
	}
}

// summaryCardFormatter 摘要卡消费侧包装（经 llmagent.WithSummaryFormatter 注入
// ContentRequestProcessor）：按 SDK 默认 formatSummary 的包裹形态包装，并在摘要
// 文本尾部拼接回溯提示 kind.SummaryRecallHint（chat↔store 跨包契约字面量，定义在
// kind 包）。hint 不持久化（store 层产纯摘要），仅注入模型请求；模型输出若复制
// hint，由 store 层持久化前剥离，不会累积。
func summaryCardFormatter(summary string) string {
	return fmt.Sprintf("Here is a brief summary of your previous interactions:\n\n"+
		"<summary_of_previous_interactions>\n%s\n</summary_of_previous_interactions>\n\n"+
		"Note: this information is from previous interactions and may be outdated. "+
		"You should ALWAYS prefer information from this conversation over the past summary.\n",
		summary+kind.SummaryRecallHint)
}

// logSummaryReport 摘要 Report 结构化落日志：触发归因（trigger/metric/value/threshold）
// 与调用记账（mode/prompt tokens/缓存命中/错误）。Trigger.Name=always 表示无 checks
// 恒 fire（当前形态），真实是否生成以 worker 阈值 gating 为准。
func logSummaryReport(_ context.Context, r summary.Report) {
	log.Infof("[Absorb] summary report | filterKey=%q trigger=%s fired=%t metric=%s value=%d threshold=%d mode=%s prompt_tokens=%d est_prompt_tokens=%d cached_tokens=%d err=%v",
		r.Trigger.FilterKey, r.Trigger.Name, r.Trigger.Fired, r.Trigger.Metric,
		r.Trigger.Value, r.Trigger.Threshold, r.Call.Mode,
		r.Call.PromptTokens, r.Call.EstimatedPromptTokens, r.Call.CachedTokens, r.Error)
}

// truncateToolResult 工具结果截断（rune 安全）。空结果返回空串（SDK 语义：跳过该结果）。
func truncateToolResult(msg model.Message) string {
	content := strings.TrimSpace(msg.Content)
	if content == "" {
		return ""
	}
	toolName := msg.ToolName
	if toolName == "" {
		toolName = "tool"
	}
	if runes := []rune(content); len(runes) > toolResultMaxRunes {
		content = string(runes[:toolResultMaxRunes]) + "…[已截断，信息不完整]"
	}
	return "[" + toolName + " 返回: " + content + "]"
}
