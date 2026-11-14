package recall

import (
	"context"
	"strings"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/session"
	"trpc.group/trpc-go/trpc-agent-go/tool"
	"trpc.group/trpc-go/trpc-agent-go/tool/function"

	"absorb-agent/internal/log"
	"absorb-agent/internal/store"
	"absorb-agent/internal/store/storage"
)

// recall_history 工具：摘要折叠后细节的按需回溯。
// index 返回某文章 scope 的对话轮次目录（当目录用），fetch 按关键词或 seq 区间
// 回拉原文切片。召回状态由 handler 每轮请求注入（WithRecallState），工具无自身字段。

// recallState 单次 /api/ask 请求内的召回状态。
type recallState struct {
	svc       *store.SessionService
	key       session.Key
	filterKey string               // 当前请求 scope（文章 URL；空 = 全会话）
	cutoffs   map[string]time.Time // 各 scope 摘要 boundary CutoffAt（covered 判定）
}

type recallStateKey struct{}

// WithRecallState 把单次请求的召回状态装入 context，供 recall_history 工具读取。
// 隐藏未导出的 recallState/recallStateKey，供 handler 注入。
func WithRecallState(ctx context.Context, svc *store.SessionService, key session.Key, filterKey string, cutoffs map[string]time.Time) context.Context {
	return context.WithValue(ctx, recallStateKey{}, &recallState{svc: svc, key: key, filterKey: filterKey, cutoffs: cutoffs})
}

func stateFromContext(ctx context.Context) *recallState {
	if v, ok := ctx.Value(recallStateKey{}).(*recallState); ok {
		return v
	}
	return nil
}

// looksLikePageURL 判断 recall_history 的 scope 入参是否为网页 URL（而非 scope 标识）。
// 带 http/https scheme 视为 URL，解析为标识；其余（16 位十六进制标识或异常输入）
// 原样使用，由存储层按标识匹配（查无即空，宁缺勿错）。
func looksLikePageURL(s string) bool {
	lower := strings.ToLower(s)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

// recallInput 工具输入。SeqRange 用切片承载 JSON 数组，len==2 视为提供了
// [fromSeq, toSeq] 闭区间（定长数组无法区分“未提供”与 [0,0]）。
type recallInput struct {
	Reason   string   `json:"reason"`
	Action   string   `json:"action"`
	Scope    string   `json:"scope,omitempty"`
	Keywords []string `json:"keywords,omitempty"`
	SeqRange []int64  `json:"seq_range,omitempty"`
}

// recallOutput 工具输出（与 expand_context 同形状：content 或 message 二选一）。
type recallOutput struct {
	Success bool   `json:"success"`
	Content string `json:"content,omitempty"`
	Message string `json:"message,omitempty"`
}

// NewRecallHistoryTool 注册 recall_history 工具。
func NewRecallHistoryTool() tool.CallableTool {
	fn := func(ctx context.Context, in recallInput) (recallOutput, error) {
		st := stateFromContext(ctx)
		if st == nil || st.svc == nil {
			return recallOutput{Success: false, Message: "召回状态不可用"}, nil
		}
		action := strings.TrimSpace(in.Action)
		if action != "index" && action != "fetch" {
			return recallOutput{Success: false, Message: "action 仅支持 index（查目录）或 fetch（拉原文切片）"}, nil
		}
		scope := strings.TrimSpace(in.Scope)
		if scope == "" {
			scope = st.filterKey
		} else if looksLikePageURL(scope) {
			// 模型从对话历史的 <page_url> 块拿到的是人类可读网址，
			// 这里解析为定长 scope 标识（全仓唯一入口 storage.ScopeID）。
			// 已是标识形态（scope 地图/目录输出）则原样使用。
			scope = storage.ScopeID(scope)
		}
		if action == "index" {
			content, err := st.svc.RecallIndex(ctx, st.key, scope, in.Keywords, st.cutoffs[scope])
			if err != nil {
				return recallOutput{Success: false, Message: "查询历史目录失败: " + err.Error()}, nil
			}
			log.Infof("[Absorb] recall_history index reason=%q scope=%q session=%s", in.Reason, scope, st.key.SessionID)
			return recallOutput{Success: true, Content: content}, nil
		}
		// fetch
		if len(in.Keywords) == 0 && len(in.SeqRange) != 2 {
			return recallOutput{Success: false, Message: "fetch 需要 keywords 或 seq_range 之一（seq_range 形如 [fromSeq, toSeq]，seq 来自 index 目录）"}, nil
		}
		if len(in.SeqRange) == 2 && in.SeqRange[0] > in.SeqRange[1] {
			return recallOutput{Success: false, Message: "seq_range 需满足 fromSeq ≤ toSeq"}, nil
		}
		content, err := st.svc.RecallFetch(ctx, st.key, scope, in.Keywords, in.SeqRange)
		if err != nil {
			return recallOutput{Success: false, Message: "回拉原文失败: " + err.Error()}, nil
		}
		log.Infof("[Absorb] recall_history fetch reason=%q scope=%q keywords=%v seq_range=%v session=%s",
			in.Reason, scope, in.Keywords, in.SeqRange, st.key.SessionID)
		return recallOutput{Success: true, Content: content}, nil
	}
	return function.NewFunctionTool(fn,
		function.WithName("recall_history"),
		function.WithDescription(
			"回溯历史对话中被摘要折叠的原文细节。仅当摘要卡提示“细节已折叠”且当前材料无法回答用户问题时调用；"+
				"两步用法：先 action=index 查看该文章的对话轮次目录（每行 seq | 作者 | 时间 | 是否已被摘要覆盖 | 内容预览），"+
				"再 action=fetch 用 keywords（原文用词，含英文原名）或 seq_range（目录中的 seq 闭区间）定点拉回相关轮次原文，基于返回原文回答。"+
				"不要盲目全量拉取：优先用精准 keywords；已覆盖的行说明细节只存在于摘要卡中，更需要回溯。"+
				"scope 不填默认当前文章；追问旧文章细节时传该文章的完整 URL（对话历史 <page_url> 块中的原值）作为 scope。"+
				"调用时不要输出任何正文说明，直接发起调用。",
		),
		function.WithInputSchema(&tool.Schema{
			Type:     "object",
			Required: []string{"reason", "action"},
			Properties: map[string]*tool.Schema{
				"reason": {
					Type:        "string",
					Description: "为什么需要回溯（一句话说明判断依据）",
				},
				"action": {
					Type:        "string",
					Description: "index=查看某 scope 的对话轮次目录；fetch=按 keywords 或 seq_range 回拉原文切片",
				},
				"scope": {
					Type:        "string",
					Description: "可选：文章完整 URL（如 https://example.com/a）或 scope 标识；不填 = 当前文章",
				},
				"keywords": {
					Type:        "array",
					Items:       &tool.Schema{Type: "string"},
					Description: "检索词数组：原文可能使用的词（含英文原名），如 [\"Runner\", \"多Agent Runner\"]。fetch 时定向检索；index 时可选，仅显示预览中含关键词的目录行",
				},
				"seq_range": {
					Type:        "array",
					Items:       &tool.Schema{Type: "integer"},
					Description: "仅 fetch：定点拉取 [fromSeq, toSeq] 闭区间（seq 取自 index 目录），如 [12, 15]",
				},
			},
		}),
	)
}
