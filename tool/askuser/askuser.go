package askuser

// ask_user 工具：向用户提问的强制挂起点（官方 Human-in-the-Loop 模式）。
// 工具声明 LongRunning 并立即返回 pending 结果，本轮 run 由 /api/ask 侧终止
// （internal/chat/handler.go drain 拦截）；用户回答经 /api/ask/answer 以官方
// 约定的更新 JSON 作为新 user message 发起续答 run（examples/humaninloop 范式）。
// 定位边界：仅用于指代无法消解需用户定夺、或 Guided 学习开场确认已有理解两种场景。

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"trpc.group/trpc-go/trpc-agent-go/tool"
	"trpc.group/trpc-go/trpc-agent-go/tool/function"

	"absorb-agent/internal/log"
)

// 工具结果状态机：pending（等待回答）→ answered（已回答）/ skipped（用户未作答转新问题）。
const (
	StatusPending  = "pending"
	StatusAnswered = "answered"
	StatusSkipped  = "skipped"
)

type input struct {
	Question string   `json:"question"`          // 问题正文：一句话，不得连环多问
	Options  []string `json:"options,omitempty"` // 候选项（候选解读/难度自评），用户也可自由文本回答
}

type PendingOutput struct {
	Status   string   `json:"status"` // 恒为 pending
	TicketID string   `json:"ticket_id"`
	Question string   `json:"question"`
	Options  []string `json:"options,omitempty"`
}

// NewAskUserTool 注册 ask_user 工具。WithLongRunning(true) 使框架把该调用标记进
// 事件 LongRunningToolIDs；pending 结果照常落库，续答由应用层编排（官方 HITL 范式）。
func NewAskUserTool() tool.CallableTool {
	fn := func(ctx context.Context, in input) (PendingOutput, error) {
		out := PendingOutput{
			Status:   StatusPending,
			TicketID: uuid.NewString(),
			Question: in.Question,
			Options:  in.Options,
		}
		log.Infof("[Absorb] ask_user pending: ticket=%s options=%d q=%d chars",
			out.TicketID, len(out.Options), len([]rune(out.Question)))
		return out, nil
	}
	return function.NewFunctionTool(fn,
		function.WithName("ask_user"),
		function.WithDescription(
			"向用户提出一个澄清或诊断问题并等待回答。仅两种场景可用："+
				"1) 用户问题的指代结合页面标题、划词与全文仍无法唯一确定（候选解读无明显主次），"+
				"需要用户定夺后再回答；2) 用户明确要求学习引导（带我理解/带我学）时，开场确认已有理解。"+
				"question 填一句话问题，options 提供候选解读（用户也可自由回答）。"+
				"一轮回答只能调用一次，禁止连环调用；其余场景一律直接回答，不要调用。",
		),
		function.WithLongRunning(true),
		function.WithInputSchema(&tool.Schema{
			Type:     "object",
			Required: []string{"question"},
			Properties: map[string]*tool.Schema{
				"question": {
					Type:        "string",
					Description: "一句话问题，不得连环多问",
				},
				"options": {
					Type:        "array",
					Items:       &tool.Schema{Type: "string"},
					Description: "2-4 个候选项（如候选解读），可省略",
				},
			},
		}),
	)
}

// ParsePending 从工具结果 content 解析 ask_user pending 提问。
// 非 ask_user 结果或非 pending 状态返回 nil（/api/ask drain 拦截用）。
func ParsePending(content string) *PendingOutput {
	var out PendingOutput
	if err := json.Unmarshal([]byte(content), &out); err != nil {
		return nil
	}
	if out.Status != StatusPending || out.TicketID == "" {
		return nil
	}
	return &out
}

// BuildAnswerUpdate 把用户回答组装成官方 HITL 更新 JSON：
// 作为下一条 user message 发起新 Run，模型据此继续回答。
func BuildAnswerUpdate(ticketID, answer string) string {
	b, _ := json.Marshal(map[string]string{
		"status":      StatusAnswered,
		"ticket_id":   ticketID,
		"user_answer": answer,
	})
	return string(b)
}

// BuildSkippedUpdate 用户未回答提问、转而发起新问题时的更新 JSON：
// 显式关闭 pending 提问，避免模型把新问题误当回答。
func BuildSkippedUpdate(ticketID string) string {
	b, _ := json.Marshal(map[string]string{
		"status":    StatusSkipped,
		"ticket_id": ticketID,
	})
	return string(b)
}
