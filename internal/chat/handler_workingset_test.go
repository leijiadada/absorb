package chat

// 全链路集成测试：工作集 L1 注入。
// 预置 A/B 两个 scope 的历史轮次 → 以 B 为当前 scope 提问 → 断言发往模型的请求体：
//  1. 跨 scope（A）轮次原文经 L1 注入（SDK scope 过滤本不可见，正是 L1 价值）；
//  2. 当前 scope（B）轮次原文恰好出现 1 次（SDK scope 重放提供，L1 去重不重复注入 → 禁双花）；
//  3. enabled=false 时 A 轮原文不出现（回退单 scope 行为）。

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/session"

	"absorb-agent/internal/conf"
	"absorb-agent/internal/store"
)

// wsSeedRound 经 SessionService 落库一个完整问答轮次（user + assistant，同 FilterKey）。
// Version 置 CurrentVersion 镜像生产事件（runner 经 event.New 创建），保证 evt.Filter 走 FilterKey。
// scope 入参是网页 URL，落库时经 store.ScopeID 哈希为定长标识——
// 与 handler 对本轮 page.url 的计算一致，否则种子事件与请求 scope 不是同一身份。
func wsSeedRound(t *testing.T, svc interface {
	AppendEvent(ctx context.Context, sess *session.Session, evt *event.Event, opts ...session.Option) error
}, sess *session.Session, userContent, answerContent, pageURL string) {
	t.Helper()
	scope := store.ScopeID(pageURL)
	for _, evt := range []*event.Event{
		{Version: event.CurrentVersion, FilterKey: scope, Response: &model.Response{
			Choices: []model.Choice{{Message: model.Message{Role: model.RoleUser, Content: userContent}}},
		}},
		{Version: event.CurrentVersion, FilterKey: scope, Response: &model.Response{
			Choices: []model.Choice{{Message: model.Message{Role: model.RoleAssistant, Content: answerContent}}},
		}},
	} {
		if err := svc.AppendEvent(context.Background(), sess, evt); err != nil {
			t.Fatalf("AppendEvent(%q): %v", userContent, err)
		}
	}
}

// wsAsk 以指定 scope 页面 URL 向 handler 提问（SSE 同步完成后返回）。
func wsAsk(t *testing.T, h http.HandlerFunc, sessionID, pageURL, question string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"question":  question,
		"sessionId": sessionID,
		"page":      map[string]any{"url": pageURL},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/ask", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec.Body.String()
}

func TestWorkingSetL1Injection(t *testing.T) {
	const sessionID = "s-l1"

	t.Run("跨scope注入且同scope不双花", func(t *testing.T) {
		h, cap2, svc := setup(t, "openai", true, false, defaultOKStream)
		key := session.Key{AppName: AppName, UserID: "absorb-user", SessionID: sessionID}
		sess, err := svc.CreateSession(context.Background(), key, nil)
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		wsSeedRound(t, svc, sess, "A的问题", "A的回答AAA", "https://a.com")
		wsSeedRound(t, svc, sess, "B的问题", "B的回答BBB", "https://b.com")

		out := wsAsk(t, h, sessionID, "https://b.com", "懂了")
		reqBody := cap2.get()
		if strings.Contains(out, `"type":"error"`) {
			t.Fatalf("不应报错，SSE: %s", out)
		}

		// A 轮（跨 scope）：经 L1 注入，恰好 1 次
		if n := strings.Count(reqBody, "A的回答AAA"); n != 1 {
			t.Errorf("A 轮回答应经 L1 注入恰好 1 次，实际 %d；请求体：%s", n, reqBody)
		}
		if n := strings.Count(reqBody, "A的问题"); n != 1 {
			t.Errorf("A 轮提问应经 L1 注入恰好 1 次，实际 %d", n)
		}
		// B 轮（当前 scope）：仅由 SDK scope 重放提供，L1 去重不得重复
		if n := strings.Count(reqBody, "B的回答BBB"); n != 1 {
			t.Errorf("B 轮回答应恰好出现 1 次（禁双花），实际 %d；请求体：%s", n, reqBody)
		}
		// 注入消息应为原生角色：A 的 user 在 assistant 之前
		ia, ib := strings.Index(reqBody, "A的问题"), strings.Index(reqBody, "A的回答AAA")
		if ia < 0 || ib < 0 || ia > ib {
			t.Errorf("L1 注入顺序应为 user 在前（ia=%d, ib=%d）", ia, ib)
		}
	})

	t.Run("开关关闭回退单scope现状", func(t *testing.T) {
		disabled := false
		wsFn := func() conf.WorkingSetConfig {
			return conf.WorkingSetConfig{Enabled: &disabled}
		}
		h, cap2, svc := setup(t, "openai", true, false, defaultOKStream, withWSFn(wsFn))
		key := session.Key{AppName: AppName, UserID: "absorb-user", SessionID: sessionID}
		sess, err := svc.CreateSession(context.Background(), key, nil)
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		wsSeedRound(t, svc, sess, "A的问题", "A的回答AAA", "https://a.com")
		wsSeedRound(t, svc, sess, "B的问题", "B的回答BBB", "https://b.com")

		out := wsAsk(t, h, sessionID, "https://b.com", "懂了")
		reqBody := cap2.get()
		if strings.Contains(out, `"type":"error"`) {
			t.Fatalf("不应报错，SSE: %s", out)
		}
		if strings.Contains(reqBody, "A的回答AAA") {
			t.Errorf("开关关闭时不应注入跨 scope 轮次")
		}
		if n := strings.Count(reqBody, "B的回答BBB"); n != 1 {
			t.Errorf("关闭后 B 轮仍应由 scope 重放提供 1 次，实际 %d", n)
		}
	})
}
