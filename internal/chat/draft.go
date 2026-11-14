package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"trpc.group/trpc-go/trpc-agent-go/model"

	"absorb-agent/internal/knowledge"
	"absorb-agent/internal/log"
	"absorb-agent/internal/store"
	"absorb-agent/internal/telemetry/tokentracker"
)

// M2 吸收闭环：知识保存草稿的 LLM 实现。与 profile 蒸馏同款模式：
// dynamicModel 解析当前默认模型（绕开 applyModelGenConfig 的聊天参数覆盖，
// 草稿自带低温/限长/关思考），一次非流式调用产出 {title, summary}。
// 不经过 runner/session——不写会话存储、不受记忆门影响。

// draftGenConfig 草稿生成参数：低温保忠实，限长控成本，关思考控延迟。
func draftGenConfig(req *model.Request) {
	disabled := false
	temp := 0.2
	maxTokens := 256
	req.GenerationConfig = model.GenerationConfig{
		Stream:          false,
		Temperature:     &temp,
		MaxTokens:       &maxTokens,
		ThinkingEnabled: &disabled,
	}
}

// draftUserPrompt 组装用户消息：问题/回答/划词摘录三段材料（可缺省）。
func draftUserPrompt(in knowledge.DraftPrompt) string {
	var b strings.Builder
	b.WriteString("【问题】\n" + in.Question + "\n\n【AI 回答】\n" + in.Answer)
	if strings.TrimSpace(in.Quote) != "" {
		b.WriteString("\n\n【原文摘录】\n" + in.Quote)
	}
	return b.String()
}

// parseDraftOutput 解析模型输出为草稿：剥代码围栏后取首尾大括号间 JSON，
// 逐字段校验类型；解析失败返回 error（调用方降级），Title 空同样视为降级信号。
func parseDraftOutput(out string) (knowledge.DraftResult, error) {
	s := strings.TrimSpace(out)
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			s = s[i : j+1]
		}
	}
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(s, "```json"), "```"))
	var res knowledge.DraftResult
	if err := json.Unmarshal([]byte(s), &res); err != nil {
		return knowledge.DraftResult{}, fmt.Errorf("draft output not json: %w", err)
	}
	return res, nil
}

// KnowledgeDraftFn 构造知识草稿的 DraftFunc（main.go 装配；每请求实时解析
// 默认模型配置，切换默认模型即时生效，无需重启）。
func KnowledgeDraftFn(sess *store.SessionService, proxyFn func() string) knowledge.DraftFunc {
	dm := newDynamicModel(sess, proxyFn)
	return func(ctx context.Context, in knowledge.DraftPrompt) (knowledge.DraftResult, error) {
		m, mc, err := dm.current(ctx)
		if err != nil {
			return knowledge.DraftResult{}, err
		}
		req := &model.Request{
			Messages: []model.Message{
				{Role: model.RoleSystem, Content: draftSystemPromptOf()},
				{Role: model.RoleUser, Content: draftUserPrompt(in)},
			},
		}
		draftGenConfig(req)
		respCh, err := m.GenerateContent(ctx, req)
		if err != nil {
			return knowledge.DraftResult{}, err
		}
		var sb strings.Builder
		var usage *model.Usage // 最终响应携带的用量（非流式调用，通常在末条响应上）
		for resp := range respCh {
			if resp == nil {
				continue
			}
			if resp.Usage != nil {
				usage = resp.Usage
			}
			for _, ch := range resp.Choices {
				sb.WriteString(ch.Message.Content)
			}
		}
		// 上报草稿 token 用量（kind=draft）：端点未回 Usage（total=0）时跳过上报并
		// 留验证日志；统计是旁路，失败不影响草稿主链路。
		if usage == nil || usage.TotalTokens == 0 {
			log.Debugf("[Absorb] knowledge draft usage missing (total=0), skip token report")
		} else {
			tokentracker.Track(tokentracker.KindDraft, mc.ModelID, tokentracker.Usage{
				PromptTokens:     usage.PromptTokens,
				CompletionTokens: usage.CompletionTokens,
				ReasoningTokens:  usage.CompletionTokensDetails.ReasoningTokens,
				TotalTokens:      usage.TotalTokens,
				CachedTokens:     usage.PromptTokensDetails.CachedTokens,
			})
		}
		out := sb.String()
		if strings.TrimSpace(out) == "" {
			return knowledge.DraftResult{}, fmt.Errorf("draft output empty")
		}
		res, err := parseDraftOutput(out)
		if err != nil {
			log.Warnf("[Absorb] knowledge draft parse failed: %v (raw: %.200s)", err, out)
			return knowledge.DraftResult{}, err
		}
		return res, nil
	}
}

// draftSystemPromptOf 系统提示词取自知识包单一来源（避免两处维护）。
func draftSystemPromptOf() string { return knowledge.DraftSystemPrompt }
