package chat

// 中断恢复（P1）集成式单测：真实 runner（NewRunner）+ httptest 假 openai 端点，
// CLI 用例覆盖全部场景（go test ./internal/chat/ -run TestResume -v）：
//  1. 无悬挂           → 正常轮：无 phase 事件、无 resumed 标记，模型调用 1 次
//  2. 悬挂 + "继续"    → 仅恢复轮：done.resumed=true，悬挂 tool_calls 被确定性补执行
//                        （补执行结果落库，会话愈合）
//  3. 悬挂 + 新问题    → 两阶段：恰 1 个 phase 事件，模型调用 2 次，问题轮请求包含
//                        愈合后的完整历史（tool round + 恢复续答 + 新问题）
//  4. 悬挂 + 空输入    → 视同"继续"（isContinueLike 空串语义）
//  5. HasDanglingToolCall 判定口径（空会话/正常轮/悬挂）
//
// 口径说明：恢复轮续答请求会包含愈合后的完整 tool round（tool_calls + 补执行结果），
// 但前提是被中断轮次有 user 消息作轮锚（生产中断轮必有提问）——内容重放按轮组织，
// 无轮锚的裸 tool_calls 事件会被挂起不重放（TestSimulateResumeWalkthrough 可复现）。
import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/session"

	"absorb-agent/internal/conf"
	"absorb-agent/internal/kind"
	"absorb-agent/internal/store"
)

// multiCapture 记录假端点收到的每一次模型请求体（两阶段编排会调模型多次）。
type multiCapture struct {
	mu     sync.Mutex
	bodies []string
}

func (m *multiCapture) add(b []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bodies = append(m.bodies, string(b))
}
func (m *multiCapture) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.bodies)
}
func (m *multiCapture) last() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.bodies) == 0 {
		return ""
	}
	return m.bodies[len(m.bodies)-1]
}

// all 返回全部模型请求体（两阶段编排的逐阶段检查用）。
func (m *multiCapture) all() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.bodies...)
}

// setupResume 与 setup 同构，但假端点用 multiCapture 记录全部调用。
func setupResume(t *testing.T, okStream string) (http.HandlerFunc, *multiCapture, *store.SessionService) {
	t.Helper()
	mc := &multiCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mc.add(b)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(okStream))
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	svc, err := store.NewSessionService(filepath.Join(dir, "session.db"))
	if err != nil {
		t.Fatalf("NewSessionService: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	ctx := context.Background()
	mcfg := &store.ModelConfig{
		ID: "m1", Provider: "openai", ModelID: "test-model",
		BaseURL: srv.URL, FullURL: true, APIKey: "test-key",
	}
	if err := svc.CreateModelConfig(ctx, mcfg); err != nil {
		t.Fatalf("CreateModelConfig: %v", err)
	}
	if err := svc.UpdateAppState(ctx, AppName, session.StateMap{"default_model_id": []byte("m1")}); err != nil {
		t.Fatalf("UpdateAppState: %v", err)
	}

	cfg := conf.Config{}
	cfg.Memory.DBPath = ":memory:"
	ps, err := NewPromptStrategy("")
	if err != nil {
		t.Fatalf("NewPromptStrategy: %v", err)
	}
	r, err := NewRunner(cfg, func() string { return "" }, ps, svc, NewMemoryGate(false))
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	reg := NewRunRegistry()
	wsFn := func() conf.WorkingSetConfig { return cfg.WorkingSet }
	h := AskHandler(r, ps, svc, AppName, "absorb-user", reg, wsFn, nil)
	return h, mc, svc
}

// seedDanglingToolCall 落库一个被中断的轮次：user 提问锚点 + assistant tool_calls（悬挂）。
// 用 recall_history：离线可执行（读会话摘要，无网络）；即使工具执行失败，结果仍会
// 作为 tool 消息落库——"补执行即愈合"不依赖工具成功。
// scope 用 kind.ScopeGeneral：生产中悬挂轮的 filterKey 与恢复轮继承的 scope 天然一致
// （RecentPageURL 从同轮历史继承）；测试无 page_url 历史，必须同用 general 才能对齐。
// user 锚点不可省：内容重放按轮组织（user 消息为轮锚），生产中断轮必有提问。
func seedDanglingToolCall(t *testing.T, svc *store.SessionService, sess *session.Session) {
	t.Helper()
	ctx := context.Background()
	if err := svc.AppendEvent(ctx, sess, &event.Event{Version: event.CurrentVersion, FilterKey: kind.ScopeGeneral, Response: &model.Response{
		Choices: []model.Choice{{Message: model.Message{Role: model.RoleUser, Content: "最初的问题"}}},
	}}); err != nil {
		t.Fatalf("AppendEvent(user): %v", err)
	}
	evt := &event.Event{Version: event.CurrentVersion, FilterKey: kind.ScopeGeneral, Response: &model.Response{
		Choices: []model.Choice{{Message: model.Message{
			Role: model.RoleAssistant,
			ToolCalls: []model.ToolCall{{
				ID:   "call-resume-1",
				Type: "function",
				Function: model.FunctionDefinitionParam{
					Name:      "recall_history",
					Arguments: []byte(`{"reason":"resume-test","action":"index","keywords":["k"]}`),
				},
			}},
		}}},
	}}
	if err := svc.AppendEvent(ctx, sess, evt); err != nil {
		t.Fatalf("AppendEvent(dangling): %v", err)
	}
}

// askSSE 向 handler 发起一次 /api/ask 并返回 SSE 输出全文（同步完成后）。
func askSSE(t *testing.T, h http.HandlerFunc, sessionID, question string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"question": question, "sessionId": sessionID})
	req := httptest.NewRequest(http.MethodPost, "/api/ask", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec.Body.String()
}

func newTestSession(t *testing.T, svc *store.SessionService, sessionID string) *session.Session {
	t.Helper()
	sess, err := svc.CreateSession(context.Background(),
		session.Key{AppName: AppName, UserID: "absorb-user", SessionID: sessionID}, nil)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return sess
}

// sessionHasHealedToolResult 校验悬挂 tool_calls 已被补执行：会话中存在配对的
// tool_result（recall_history 真实输出），即"补执行即愈合"已落账。
func sessionHasHealedToolResult(t *testing.T, svc *store.SessionService, sessionID string) bool {
	t.Helper()
	sess, err := svc.GetSession(context.Background(),
		session.Key{AppName: AppName, UserID: "absorb-user", SessionID: sessionID})
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	sess.EventMu.RLock()
	defer sess.EventMu.RUnlock()
	for _, e := range sess.Events {
		if e.Response != nil && e.Response.IsToolResultResponse() {
			return true
		}
	}
	return false
}

func TestHasDanglingToolCall(t *testing.T) {
	svc, err := store.NewSessionService(filepath.Join(t.TempDir(), "session.db"))
	if err != nil {
		t.Fatalf("NewSessionService: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	ctx := context.Background()

	t.Run("空会话为假", func(t *testing.T) {
		newTestSession(t, svc, "d-empty")
		ok, err := svc.HasDanglingToolCall(ctx, AppName, "absorb-user", "d-empty")
		if err != nil || ok {
			t.Errorf("空会话应返回 false,nil，got %v,%v", ok, err)
		}
	})
	t.Run("正常轮为假", func(t *testing.T) {
		sess := newTestSession(t, svc, "d-normal")
		svc.AppendEvent(ctx, sess, &event.Event{Version: event.CurrentVersion, Response: &model.Response{
			Choices: []model.Choice{{Message: model.Message{Role: model.RoleAssistant, Content: "完整回答"}}},
		}})
		ok, err := svc.HasDanglingToolCall(ctx, AppName, "absorb-user", "d-normal")
		if err != nil || ok {
			t.Errorf("正常轮应返回 false,nil，got %v,%v", ok, err)
		}
	})
	t.Run("悬挂_tool_calls_为真", func(t *testing.T) {
		sess := newTestSession(t, svc, "d-dangling")
		seedDanglingToolCall(t, svc, sess)
		ok, err := svc.HasDanglingToolCall(ctx, AppName, "absorb-user", "d-dangling")
		if err != nil || !ok {
			t.Errorf("悬挂应返回 true,nil，got %v,%v", ok, err)
		}
	})
}

func TestResumeNoDanglingNormalTurn(t *testing.T) {
	h, mc, _ := setupResume(t, defaultOKStream)
	out := askSSE(t, h, "s-normal", "这个问题没有历史")
	if strings.Contains(out, `"type":"phase"`) {
		t.Errorf("无悬挂不应有 phase 事件，SSE: %s", out)
	}
	if strings.Contains(out, `"resumed":true`) {
		t.Errorf("无悬挂不应有 resumed 标记，SSE: %s", out)
	}
	if !strings.Contains(out, `"done":true`) {
		t.Errorf("应正常 done，SSE: %s", out)
	}
	if mc.count() != 1 {
		t.Errorf("模型应被调用 1 次，实际 %d", mc.count())
	}
}

func TestResumeContinueLike(t *testing.T) {
	h, mc, svc := setupResume(t, defaultOKStream)
	sess := newTestSession(t, svc, "s-resume-cont")
	seedDanglingToolCall(t, svc, sess)

	out := askSSE(t, h, "s-resume-cont", "继续")

	if strings.Contains(out, `"type":"phase"`) {
		t.Errorf("continue-like 只应有恢复轮，不应有 phase 事件，SSE: %s", out)
	}
	if !strings.Contains(out, `"resumed":true`) {
		t.Errorf("done 应带 resumed 标记，SSE: %s", out)
	}
	if !strings.Contains(out, `"done":true`) {
		t.Errorf("应下发 done，SSE: %s", out)
	}
	if strings.Contains(out, `"type":"error"`) {
		t.Errorf("恢复轮不应报错，SSE: %s", out)
	}
	// 模型仅恢复轮一次调用（空消息续答）
	if mc.count() != 1 {
		t.Errorf("continue-like 模型应被调用 1 次，实际 %d", mc.count())
	}
	// 确定性补执行已落账：悬挂 tool_calls 的配对工具结果进入会话（愈合）
	if !sessionHasHealedToolResult(t, svc, "s-resume-cont") {
		t.Errorf("恢复轮应补执行悬挂工具并把结果落库（会话中应有 tool_result）")
	}
}

func TestResumeNewQuestionTwoPhase(t *testing.T) {
	h, mc, svc := setupResume(t, defaultOKStream)
	sess := newTestSession(t, svc, "s-resume-q")
	seedDanglingToolCall(t, svc, sess)

	out := askSSE(t, h, "s-resume-q", "这篇讲了什么？")

	if n := strings.Count(out, `"type":"phase"`); n != 1 {
		t.Errorf("两阶段应恰有 1 个 phase 事件，实际 %d，SSE: %s", n, out)
	}
	if !strings.Contains(out, `"done":true`) {
		t.Errorf("应下发 done，SSE: %s", out)
	}
	if strings.Contains(out, `"type":"error"`) {
		t.Errorf("两阶段不应报错，SSE: %s", out)
	}
	// 模型两次调用：恢复轮续答 + 用户问题轮
	if mc.count() != 2 {
		t.Errorf("两阶段模型应被调用 2 次，实际 %d", mc.count())
	}
	// 问题轮请求包含愈合后的完整历史：悬挂 tool round（补执行结果）+ 恢复续答 + 新问题
	last := mc.last()
	if !strings.Contains(last, "call-resume-1") {
		t.Errorf("问题轮请求应包含愈合后的 tool round（call-resume-1），请求体：%s", clipForTest(last))
	}
	if !strings.Contains(last, "这篇讲了什么") {
		t.Errorf("问题轮请求应包含用户新问题，请求体：%s", clipForTest(last))
	}
}

func TestResumeEmptyInputAsContinue(t *testing.T) {
	h, mc, svc := setupResume(t, defaultOKStream)
	sess := newTestSession(t, svc, "s-resume-empty")
	seedDanglingToolCall(t, svc, sess)

	out := askSSE(t, h, "s-resume-empty", "")

	if strings.Contains(out, `"type":"phase"`) {
		t.Errorf("空输入视同继续，不应有 phase 事件，SSE: %s", out)
	}
	if !strings.Contains(out, `"resumed":true`) {
		t.Errorf("空输入应走恢复轮（done.resumed=true），SSE: %s", out)
	}
	if mc.count() != 1 {
		t.Errorf("空输入模型应被调用 1 次，实际 %d", mc.count())
	}
	if !sessionHasHealedToolResult(t, svc, "s-resume-empty") {
		t.Errorf("空输入恢复轮同样应补执行悬挂工具（会话中应有 tool_result）")
	}
}

// clipForTest 请求体截断（测试失败信息可读性）。
func clipForTest(s string) string {
	if len(s) <= 800 {
		return s
	}
	return s[:800] + "..."
}

// TestSimulateResumeWalkthrough 主动模拟：完整走一遍"中断 → 继续 → 恢复"，
// dump 每个阶段模型实际收到的请求与最终 session 状态（演示用，保留作活文档）。
func TestSimulateResumeWalkthrough(t *testing.T) {
	// 恢复轮续答流（Phase A 模型返回的内容）
	continueStream := "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"刚才说到一半：这篇文章的论点需要结合目录看\"},\"finish_reason\":null}]}\n\n" +
		"data: [DONE]\n\n"
	h, mc, svc := setupResume(t, continueStream)
	sess := newTestSession(t, svc, "s-sim")
	// 中断现场：用户问"总结这篇文章" → 模型决定先查历史摘要 → 此刻用户点了停止
	seedDanglingToolCall(t, svc, sess)

	dumpEvents := func(tag string) {
		s, _ := svc.GetSession(context.Background(),
			session.Key{AppName: AppName, UserID: "absorb-user", SessionID: "s-sim"})
		s.EventMu.RLock()
		defer s.EventMu.RUnlock()
		var lines []string
		for i, e := range s.Events {
			switch {
			case e.Response == nil:
				lines = append(lines, fmt.Sprintf("  [%d] (无响应体)", i))
			case e.Response.IsToolCallResponse():
				lines = append(lines, fmt.Sprintf("  [%d] assistant tool_calls id=%s name=%s", i, "call-resume-1", "recall_history"))
			case e.Response.IsToolResultResponse():
				lines = append(lines, fmt.Sprintf("  [%d] tool 结果（配对 call-resume-1）", i))
			case len(e.Response.Choices) > 0:
				lines = append(lines, fmt.Sprintf("  [%d] %s: %s", i, e.Response.Choices[0].Message.Role, clipForTest(e.Response.Choices[0].Message.Content)))
			default:
				lines = append(lines, fmt.Sprintf("  [%d] (完成快照)", i))
			}
		}
		t.Logf("【%s】session 事件序列：\n%s", tag, strings.Join(lines, "\n"))
	}

	dumpRequest := func(tag string, body string) {
		var pretty map[string]any
		_ = json.Unmarshal([]byte(body), &pretty)
		msgs, _ := pretty["messages"].([]any)
		var lines []string
		for _, m := range msgs {
			mm := m.(map[string]any)
			role, _ := mm["role"].(string)
			if role == "" {
				role = "system"
			}
			c, _ := mm["content"].(string)
			if tc, ok := mm["tool_calls"]; ok {
				lines = append(lines, fmt.Sprintf("  %s: (tool_calls recall_history)", role))
				_ = tc
				continue
			}
			if role == "tool" {
				lines = append(lines, fmt.Sprintf("  %s: %s", role, clipForTest(c)))
				continue
			}
			lines = append(lines, fmt.Sprintf("  %s: %s", role, clipForTest(c)))
		}
		t.Logf("【%s】模型收到 %d 条消息：\n%s", tag, len(msgs), strings.Join(lines, "\n"))
	}

	// 用户回来，输入的是一个新问题（走两阶段）
	out := askSSE(t, h, "s-sim", "这篇的核心论点是什么？")
	if strings.Contains(out, `"type":"error"`) {
		t.Fatalf("模拟不应报错，SSE: %s", out)
	}
	t.Logf("【SSE 输出】包含 phase 事件=%v，resumed 完成=%v",
		strings.Contains(out, `"type":"phase"`), !strings.Contains(out, `"type":"phase"`))
	for i, b := range mc.all() {
		tag := "Phase A 恢复轮：补执行后续答"
		if i == 1 {
			tag = "Phase B 问题轮：回答新问题"
		}
		dumpRequest(tag, b)
	}
	dumpEvents("结束后最终状态")
}
