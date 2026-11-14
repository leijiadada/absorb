package chat

// 工作集上下文：把全局最近的完整问答轮次注入每轮请求（不落库、逐 run 提供，
// 经 agent.WithLateContextMessages 自动插到最新 user 轮之前）。
// 去重与预算规则基于 SDK 的行为事实：
//   - 分支过滤默认 Prefix 模式 → evt.Filter(filterKey)：filterKey 或事件 FilterKey 为空即纳入；
//   - 同 run 全部事件共享同一 FilterKey（InjectIntoEvent 统一打标），一轮不会跨 scope；
//   - handler 恒打标（文章哈希或 kind.ScopeGeneral），不再产生空标事件；无页面轮次
//     落通用 scope，对文章请求 Filter=false → 由此注入（跨文章连续性），对 general
//     请求 Filter=true → 丢弃（SDK 已原样重放同 scope 历史）。

import (
	"fmt"
	"unicode/utf8"

	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/model"

	"absorb-agent/internal/store/storage"
)

// l1Stats L1 组装统计（用于日志）。日志不输出消息原文，仅计数与估算值。
type l1Stats struct {
	Selected   int // 存储返回的完整轮次数
	Kept       int // 去重后保留并注入的轮次数
	DroppedDup int // 去重丢弃的轮次数（与当前 scope 请求历史重复）
	Messages   int // 注入消息条数（每轮 2 条：user + assistant）
	EstTokens  int // 注入内容估算 token（启发式）
}

// estimateTokens 估算 token 数（启发式：CJK 偏保守按 2/3 rune 折算）。
// 仅用于预算闸门与日志，非精确计量。
func estimateTokens(s string) int {
	r := utf8.RuneCountInString(s)
	return (r*2 + 2) / 3
}

// buildWorkingSetL1 由原始轮次（升序输入）组装 L1 注入消息（升序输出）：
//
//   - 去重：一轮若 UserEvent.Filter(当前 filterKey) 为真，说明该轮已被 SDK 按 scope
//     原样重放（cutoff 后）或已被摘要卡覆盖（cutoff 前），再注入即重复，整轮丢弃；
//     分支外轮次保留——L1 的价值正是补上跨 scope 的对话连续性（含 general 轮对
//     文章请求的注入）。filterKey 为空时 SDK 本就重放全会话，全部丢弃（防御分支，
//     当前调用方恒传非空标）。
//
//   - 超预算：估算 token 超过 maxTokens 时返回可读错误，不做裁剪。
//
// keepImages 控制注入消息是否保留 image part（库内形态=落库缩略图，见
// store/sqlite/thumbnail.go）：按默认模型视觉能力门控（调用点传 mc.ImageSupport），
// 非视觉模型不注入。兜底链：openai 层 WithTextOnlyMessageContent 本就会对非视觉
// 模型剥离 image part，本参数属防御层（不依赖框架行为 + 省去无谓负载）。
//
// rounds 为空或全部去重丢弃时返回 (nil, stats, nil)。
func buildWorkingSetL1(rounds []storage.RecentRound, filterKey string, maxTokens int, keepImages bool) ([]model.Message, l1Stats, error) {
	stats := l1Stats{Selected: len(rounds)}
	kept := make([]storage.RecentRound, 0, len(rounds))
	for _, rd := range rounds {
		if rd.UserEvent.Filter(filterKey) {
			stats.DroppedDup++
			continue
		}
		kept = append(kept, rd)
	}
	stats.Kept = len(kept)
	if len(kept) == 0 {
		return nil, stats, nil
	}

	msgs := make([]model.Message, 0, len(kept)*2)
	for _, rd := range kept {
		um := l1UserMessage(&rd.UserEvent, keepImages)
		am := l1AssistantMessage(&rd.FinalEvent, rd.ToolCount)
		msgs = append(msgs, um, am)
		stats.EstTokens += estimateTokens(um.Content) + estimateTokens(am.Content)
	}
	stats.Messages = len(msgs)
	if maxTokens > 0 && stats.EstTokens > maxTokens {
		return nil, stats, fmt.Errorf("最近对话（L1）估算约 %d tokens，超出预算 %d；请开启新会话或稍后重试", stats.EstTokens, maxTokens)
	}
	return msgs, stats, nil
}

// l1UserMessage 由用户事件还原原生 user 消息。
// image part（库内形态=落库缩略图，见 store/sqlite/thumbnail.go）按 keepImages 门控：
// 视觉模型保留（跨轮视觉参考），非视觉模型过滤；保留时在文本前缀注入图片存在性标记，
// 该标记随文本自然流进 L2/L3 摘要——与 Trae 压缩后「图片仅剩文字提及」同构。
func l1UserMessage(evt *event.Event, keepImages bool) model.Message {
	m := evt.Response.Choices[0].Message
	out := model.Message{Role: model.RoleUser, Content: m.Content}
	imgCount := 0
	for _, p := range m.ContentParts {
		if p.Type == model.ContentType("image") {
			if keepImages {
				out.ContentParts = append(out.ContentParts, p)
				imgCount++
			}
			continue
		}
		out.ContentParts = append(out.ContentParts, p)
	}
	if imgCount > 0 {
		out.Content = fmt.Sprintf("[本轮含 %d 张图片（缩略图，已降采样）]\n\n%s", imgCount, out.Content)
	}
	return out
}

// l1AssistantMessage 由最终 assistant 事件还原原生 assistant 消息：
// 不回放 ReasoningContent（历史思考，回放违反厂商协议惯例）与 ToolCalls
// （工具轮已折叠）。轮内含工具调用时，最终文本前缀一行折叠标记（合并进
// 同一消息而非独立消息，规避部分 OpenAI 兼容端点对连续 assistant 消息的兼容风险）。
func l1AssistantMessage(evt *event.Event, toolCount int) model.Message {
	content := evt.Response.Choices[0].Message.Content
	if toolCount > 0 {
		content = fmt.Sprintf("（该轮含 %d 次工具调用，详情可回溯）\n\n%s", toolCount, content)
	}
	return model.Message{Role: model.RoleAssistant, Content: content}
}
