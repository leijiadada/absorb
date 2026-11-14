package chat

// ask_user 挂起全链路集成测试（真实 runner + 假模型端点，官方 HITL 范式）：
//  1. /api/ask：模型调 ask_user → 工具返回 pending → SSE 下发卡片帧 → done(pending:true)，
//     本轮被 Cancel 终止，卡片帧之后的任何模型输出不得外显；
//  2. /api/ask/answer：用户回答组装成更新 JSON 作为新 user message 发起续答 run，
//     续答 token 流式下发，done 正常收尾。
//
// 断言的是接线行为（帧形状/终止语义/续答通路），模型决策质量由评测体系覆盖。
import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/session"

	"absorb-agent/internal/conf"
	"absorb-agent/internal/store"
)

// askUserCapture 记录假端点按次收到的请求体（ask_user 全链路有多次 LLM 调用）。
type askUserCapture struct {
	mu     sync.Mutex
	bodies []string
}

func (c *askUserCapture) add(b string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bodies = append(c.bodies, b)
}

func (c *askUserCapture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bodies)
}

// llmCallsContaining 返回包含指定子串的请求体列表（按 openai 请求体特征过滤）。
func (c *askUserCapture) llmCallsContaining(sub string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, b := range c.bodies {
		if strings.Contains(b, sub) {
			out = append(out, b)
		}
	}
	return out
}

// toolCallsStream ask_user 工具调用流：delta.tool_calls + finish_reason=tool_calls。
const toolCallsStream = "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_ask_1\",\"type\":\"function\",\"function\":{\"name\":\"ask_user\",\"arguments\":\"{\\\"question\\\":\\\"你想了解哪种存储引擎的问题？\\\",\\\"options\\\":[\\\"B+ 树\\\",\\\"LSM 树\\\"]}\"}}]},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
	"data: [DONE]\n\n"

// answerStream 续答轮的正常内容流。
const answerStream = "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"好的，聚焦 LSM 树：它的代价是读放大与写放大。\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: [DONE]\n\n"

// setupAskUser 构建假端点：第 1 次 LLM 调用返回 ask_user 工具调用，其后所有调用
// 返回续答内容流。返回 /api/ask 与 /api/ask/answer 两个 handler 及请求捕获器。
func setupAskUser(t *testing.T) (askH, answerH http.HandlerFunc, svc *store.SessionService, cap *askUserCapture) {
	t.Helper()
	c := &askUserCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.add(string(b))
		w.Header().Set("Content-Type", "text/event-stream")
		if c.count() == 1 {
			_, _ = io.WriteString(w, toolCallsStream)
			return
		}
		_, _ = io.WriteString(w, answerStream)
	}))
	t.Cleanup(srv.Close)

	svc, err := store.NewSessionService(filepath.Join(t.TempDir(), "session.db"))
	if err != nil {
		t.Fatalf("NewSessionService: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	ctx := context.Background()
	mc := &store.ModelConfig{
		ID: "m1", Provider: "openai", ModelID: "test-model",
		BaseURL: srv.URL, FullURL: true, APIKey: "test-key",
	}
	if err := svc.CreateModelConfig(ctx, mc); err != nil {
		t.Fatalf("CreateModelConfig: %v", err)
	}
	if err := svc.UpdateAppState(ctx, AppName, session.StateMap{"default_model_id": []byte("m1")}); err != nil {
		t.Fatalf("UpdateAppState: %v", err)
	}

	cfg := conf.Config{}
	cfg.Memory.DBPath = ":memory:"
	cfg.Fetch.TimeoutSeconds = 30
	ps, err := NewPromptStrategy("")
	if err != nil {
		t.Fatalf("NewPromptStrategy: %v", err)
	}
	r, err := NewRunner(cfg, func() string { return "" }, ps, svc, NewMemoryGate(false))
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	reg := NewRunRegistry()
	askH = AskHandler(r, ps, svc, AppName, "absorb-user", reg,
		func() conf.WorkingSetConfig { return conf.WorkingSetConfig{} }, nil)
	answerH = AnswerHandler(r, ps, svc, AppName, "absorb-user", reg)
	return askH, answerH, svc, c
}

// postSSE 发起 POST 请求并返回 SSE 输出全文。
func postSSE(t *testing.T, h http.HandlerFunc, path string, body map[string]any) string {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec.Body.String()
}

// extractAskUserCard 从 SSE 全文取 ask_user 卡片帧（无则返回 nil）。
func extractAskUserCard(t *testing.T, sse string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(sse, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var evt map[string]any
		if err := json.Unmarshal([]byte(line[6:]), &evt); err != nil {
			continue
		}
		if evt["type"] == "ask_user" {
			return evt
		}
	}
	return nil
}

func TestAskUser_PendingFlow(t *testing.T) {
	askH, _, _, _ := setupAskUser(t)
	sse := postSSE(t, askH, "/api/ask", map[string]any{"question": "这个有什么问题？"})

	card := extractAskUserCard(t, sse)
	if card == nil {
		t.Fatalf("应下发 ask_user 卡片帧。SSE: %s", sse)
	}
	if card["ticketId"] == "" || card["question"] == "" {
		t.Errorf("卡片帧应携带 ticketId/question: %v", card)
	}
	opts, _ := card["options"].([]any)
	if len(opts) != 2 {
		t.Errorf("卡片帧应携带 options: %v", card)
	}
	if !strings.Contains(sse, `"pending":true`) {
		t.Errorf("done 应带 pending:true 标记。SSE: %s", sse)
	}
	// 卡片帧之后不得外显任何续答 token（提前终止语义）
	if strings.Contains(sse, "聚焦 LSM 树") {
		t.Errorf("卡片帧之后不应有模型补讲内容外显。SSE: %s", sse)
	}
}

func TestAskUser_PendingRoundNoDangling(t *testing.T) {
	// 回归：pending 轮结束后会话不得悬挂 tool_calls——若工具结果未落库，
	// 该会话后续所有消息都会触发模型端 400（实测事故根因）
	askH, _, svc, _ := setupAskUser(t)
	_ = postSSE(t, askH, "/api/ask", map[string]any{
		"question":  "这个有什么问题？",
		"sessionId": askSessionID,
	})
	dangling, err := svc.HasDanglingToolCall(context.Background(), AppName, "absorb-user", askSessionID)
	if err != nil {
		t.Fatalf("HasDanglingToolCall: %v", err)
	}
	if dangling {
		t.Errorf("pending 轮结束后不应悬挂 tool_calls（工具结果必须落库）")
	}
}

// askSessionID 显式会话 ID：ask 与 answer 两轮共用，模拟前端持有的会话。
const askSessionID = "s-askuser-fixed"

func TestAskUser_AnswerContinuation(t *testing.T) {
	askH, answerH, _, cap := setupAskUser(t)
	sse1 := postSSE(t, askH, "/api/ask", map[string]any{
		"question":  "这个有什么问题？",
		"sessionId": askSessionID,
	})
	card := extractAskUserCard(t, sse1)
	if card == nil {
		t.Fatalf("前置：第一轮应下发卡片帧。SSE: %s", sse1)
	}
	ticket, _ := card["ticketId"].(string)

	sse2 := postSSE(t, answerH, "/api/ask/answer", map[string]any{
		"sessionId": askSessionID,
		"ticketId":  ticket,
		"answer":    "LSM 树",
	})
	if !strings.Contains(sse2, "聚焦 LSM 树") {
		t.Errorf("续答应流式返回内容。SSE: %s", sse2)
	}
	if !strings.Contains(sse2, `"done":true`) || strings.Contains(sse2, `"pending":true`) {
		t.Errorf("续答 done 应为正常收尾（无 pending 标记）。SSE: %s", sse2)
	}
	// 续答 Run 应携带官方 HITL 更新 JSON（answered 状态 + user_answer）。
	// 请求体是 JSON-in-JSON 的转义形态，用不带引号的字段名匹配。
	if len(cap.llmCallsContaining("user_answer")) == 0 {
		t.Errorf("续答 Run 应携带 answered 更新 JSON 作为 user message")
	}
}

func TestAskUser_AnswerValidation(t *testing.T) {
	_, answerH, _, _ := setupAskUser(t)
	t.Run("missing_answer", func(t *testing.T) {
		out := postSSE(t, answerH, "/api/ask/answer", map[string]any{"sessionId": "s1", "answer": ""})
		if !strings.Contains(out, "不能为空") {
			t.Errorf("空 answer 应返回校验错误。SSE: %s", out)
		}
	})
	t.Run("session_missing", func(t *testing.T) {
		out := postSSE(t, answerH, "/api/ask/answer", map[string]any{
			"sessionId": "s-not-exist", "ticketId": "t1", "answer": "LSM 树",
		})
		if !strings.Contains(out, "会话不存在或已清理") {
			t.Errorf("会话不存在应返回 L1 校验错误。SSE: %s", out)
		}
	})
}

// 编译期锚定：续答轮消息必须为 user role（官方 HITL 更新消息形态）。
var _ = model.NewUserMessage
