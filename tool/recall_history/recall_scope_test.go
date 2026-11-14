package recall

// recall_history 工具 scope 入参解析测试：
// 模型从对话历史的 <page_url> 块拿到的是人类可读网址，工具必须解析为定长
// scope 标识才能命中哈希打标的事件；已是标识形态则原样使用；不填取当前 scope。

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/session"

	"absorb-agent/internal/store"
)

func newRecallToolTestService(t *testing.T) *store.SessionService {
	t.Helper()
	svc, err := store.NewSessionService(filepath.Join(t.TempDir(), "recall-tool.db"))
	if err != nil {
		t.Fatalf("NewSessionService: %v", err)
	}
	t.Cleanup(func() { svc.Close() })
	return svc
}

func seedRecallScope(t *testing.T, svc *store.SessionService, sess *session.Session, pageURL, q, a string) {
	t.Helper()
	scope := store.ScopeID(pageURL)
	base := time.Now().Add(-time.Hour)
	for _, evt := range []*event.Event{
		{Version: event.CurrentVersion, FilterKey: scope, Timestamp: base, Response: &model.Response{
			Choices: []model.Choice{{Message: model.Message{Role: model.RoleUser, Content: q}}},
		}},
		{Version: event.CurrentVersion, FilterKey: scope, Timestamp: base.Add(time.Minute), Response: &model.Response{
			Choices: []model.Choice{{Message: model.Message{Role: model.RoleAssistant, Content: a}}},
		}},
	} {
		if err := svc.AppendEvent(context.Background(), sess, evt); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}
}

// TestRecallHistoryTool_ResolvesURLScope scope 传文章 URL 时应解析为标识并命中事件；
// 传标识原样命中；传空用当前 scope。
func TestRecallHistoryTool_ResolvesURLScope(t *testing.T) {
	svc := newRecallToolTestService(t)
	ctx := context.Background()
	key := session.Key{AppName: "absorb", UserID: "u1", SessionID: "s-recall-tool"}
	if _, err := svc.CreateSession(ctx, key, nil); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sess, err := svc.GetSession(ctx, key)
	if err != nil || sess == nil {
		t.Fatalf("GetSession: sess=%v err=%v", sess, err)
	}
	const (
		pageA = "https://a.example.com/article"
		pageB = "https://b.example.com/article"
	)
	seedRecallScope(t, svc, sess, pageA, "A的特有问题", "A的特有回答")
	seedRecallScope(t, svc, sess, pageB, "B的特有问题", "B的特有回答")

	// 当前 scope = B；工具调用时 scope 传 A 的 URL（模型从历史 <page_url> 复制）
	toolCtx := WithRecallState(ctx, svc, key, store.ScopeID(pageB), map[string]time.Time{})
	tool := NewRecallHistoryTool()

	// scope 传 URL → 解析为标识 → 命中 A，不串 B
	out, err := tool.Call(toolCtx, []byte(`{"reason":"验证URL解析","action":"index","scope":"`+pageA+`"}`))
	if err != nil {
		t.Fatalf("tool.Call(url): %v", err)
	}
	text, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(text), "A的特有回答") {
		t.Fatalf("scope 传 URL 应解析为标识并命中 A 的事件，输出：%s", text)
	}
	if strings.Contains(string(text), "B的特有回答") {
		t.Fatalf("不应串入 B 的事件，输出：%s", text)
	}

	// scope 传标识 → 原样命中
	out2, err := tool.Call(toolCtx, []byte(`{"reason":"验证标识直传","action":"index","scope":"`+store.ScopeID(pageA)+`"}`))
	if err != nil {
		t.Fatalf("tool.Call(id): %v", err)
	}
	text2, _ := json.Marshal(out2)
	if !strings.Contains(string(text2), "A的特有回答") {
		t.Fatalf("scope 传标识应原样命中，输出：%s", text2)
	}

	// scope 不填 → 当前 scope（B）
	out3, err := tool.Call(toolCtx, []byte(`{"reason":"验证默认scope","action":"index"}`))
	if err != nil {
		t.Fatalf("tool.Call(default): %v", err)
	}
	text3, _ := json.Marshal(out3)
	if !strings.Contains(string(text3), "B的特有回答") {
		t.Fatalf("默认应取当前 scope（B），输出：%s", text3)
	}
}
