package sqlite

// RecentGlobalRounds（工作集 L1 数据源）单元测试：
// 轮次配对 / 工具计数 / 悬挂 user 跳过 / 跨 scope 全返回 / 坏 JSON 行跳过 / maxRounds 截断。
// 事件构造仿 kv_test.go 惯例（直接构造 event.Event，seq 由 AppendEvent 递增）。

import (
	"context"
	"path/filepath"
	"testing"

	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/session"

	"absorb-agent/internal/store/storage"
)

func newRoundsTestStore(t *testing.T) *sqliteStore {
	t.Helper()
	raw, err := newSQLite(filepath.Join(t.TempDir(), "rounds.db"))
	if err != nil {
		t.Fatalf("newSQLite: %v", err)
	}
	st := raw.(*sqliteStore) // newSQLite 返回接口，测试需具体类型以直插坏 JSON 行
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// roundsSeed 追加一条 role=role、content=content、FilterKey=scope 的事件。
func roundsSeed(t *testing.T, st *sqliteStore, ctx context.Context, s *session.Session, role model.Role, content, scope string, toolCalls []model.ToolCall) {
	t.Helper()
	msg := model.Message{Role: role, Content: content, ToolCalls: toolCalls}
	if role == model.RoleTool {
		msg.ToolID = "c1"
	}
	evt := &event.Event{Version: event.CurrentVersion, FilterKey: scope, Response: &model.Response{
		Choices: []model.Choice{{Message: msg}},
	}}
	if err := st.AppendEvent(ctx, s, evt); err != nil {
		t.Fatalf("AppendEvent(%s): %v", content, err)
	}
}

func TestRecentGlobalRounds(t *testing.T) {
	ctx := context.Background()
	key := session.Key{AppName: "app", UserID: "user", SessionID: "s-rounds"}

	mustSession := func(st *sqliteStore) *session.Session {
		t.Helper()
		s, err := st.CreateSession(ctx, key, nil)
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		return s
	}

	t.Run("两轮纯文本升序返回", func(t *testing.T) {
		st := newRoundsTestStore(t)
		s := mustSession(st)
		roundsSeed(t, st, ctx, s, model.RoleUser, "第一问", "https://a.com", nil)
		roundsSeed(t, st, ctx, s, model.RoleAssistant, "第一答", "https://a.com", nil)
		roundsSeed(t, st, ctx, s, model.RoleUser, "第二问", "https://b.com", nil)
		roundsSeed(t, st, ctx, s, model.RoleAssistant, "第二答", "https://b.com", nil)

		got, err := st.RecentGlobalRounds(ctx, key, 2)
		if err != nil {
			t.Fatalf("RecentGlobalRounds: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("rounds = %d, want 2", len(got))
		}
		if got[0].UserEvent.Response.Choices[0].Message.Content != "第一问" ||
			got[0].FinalEvent.Response.Choices[0].Message.Content != "第一答" {
			t.Errorf("round0 配对错误: %+v", got[0])
		}
		if got[1].UserEvent.Response.Choices[0].Message.Content != "第二问" ||
			got[1].FinalEvent.Response.Choices[0].Message.Content != "第二答" {
			t.Errorf("round1 配对错误: %+v", got[1])
		}
		if got[0].ToolCount != 0 || got[1].ToolCount != 0 {
			t.Errorf("纯文本轮 ToolCount 应为 0: %d/%d", got[0].ToolCount, got[1].ToolCount)
		}
		// 跨 scope 不在本层过滤（去重由调用方按分支判定），FilterKey 原样保留
		if got[0].UserEvent.FilterKey != "https://a.com" || got[1].UserEvent.FilterKey != "https://b.com" {
			t.Errorf("FilterKey 应原样保留: %q / %q", got[0].UserEvent.FilterKey, got[1].UserEvent.FilterKey)
		}
	})

	t.Run("工具轮计数与悬挂user跳过", func(t *testing.T) {
		st := newRoundsTestStore(t)
		s := mustSession(st)
		roundsSeed(t, st, ctx, s, model.RoleUser, "工具问", "https://a.com", nil)
		roundsSeed(t, st, ctx, s, model.RoleAssistant, "", "https://a.com", []model.ToolCall{
			{ID: "c1", Type: "function", Function: model.FunctionDefinitionParam{Name: "recall_history"}},
		})
		roundsSeed(t, st, ctx, s, model.RoleTool, "工具结果", "https://a.com", nil)
		roundsSeed(t, st, ctx, s, model.RoleAssistant, "工具答", "https://a.com", nil)
		roundsSeed(t, st, ctx, s, model.RoleUser, "取消轮悬挂问", "https://a.com", nil) // 无 assistant，不计入

		got, err := st.RecentGlobalRounds(ctx, key, 2)
		if err != nil {
			t.Fatalf("RecentGlobalRounds: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("rounds = %d, want 1（悬挂 user 不构成完整轮次）", len(got))
		}
		if got[0].FinalEvent.Response.Choices[0].Message.Content != "工具答" {
			t.Errorf("FinalEvent = %q, want 工具答", got[0].FinalEvent.Response.Choices[0].Message.Content)
		}
		if got[0].ToolCount != 2 { // 1 次工具调用 + 1 次工具结果
			t.Errorf("ToolCount = %d, want 2", got[0].ToolCount)
		}
	})

	t.Run("maxRounds截断与上限保护", func(t *testing.T) {
		st := newRoundsTestStore(t)
		s := mustSession(st)
		for i := 0; i < 3; i++ {
			roundsSeed(t, st, ctx, s, model.RoleUser, "问", "https://a.com", nil)
			roundsSeed(t, st, ctx, s, model.RoleAssistant, "答", "https://a.com", nil)
		}
		got, err := st.RecentGlobalRounds(ctx, key, 1)
		if err != nil {
			t.Fatalf("RecentGlobalRounds: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("rounds = %d, want 1", len(got))
		}
		if got[0].FinalEvent.Response.Choices[0].Message.Content != "答" {
			t.Errorf("应取最新一轮")
		}
		// maxRounds <= 0 直接返回空
		if zero, err := st.RecentGlobalRounds(ctx, key, 0); err != nil || len(zero) != 0 {
			t.Errorf("maxRounds=0 应返回空: %v %v", zero, err)
		}
	})

	t.Run("坏JSON行跳过", func(t *testing.T) {
		st := newRoundsTestStore(t)
		s := mustSession(st)
		roundsSeed(t, st, ctx, s, model.RoleUser, "前问", "https://a.com", nil)
		roundsSeed(t, st, ctx, s, model.RoleAssistant, "前答", "https://a.com", nil)
		if _, err := st.db.Exec(
			`INSERT INTO session_events(app_name, user_id, session_id, seq, event) VALUES(?,?,?,?,?)`,
			key.AppName, key.UserID, key.SessionID, 99, "{bad json"); err != nil {
			t.Fatalf("seed bad row: %v", err)
		}
		roundsSeed(t, st, ctx, s, model.RoleUser, "后问", "https://b.com", nil)
		roundsSeed(t, st, ctx, s, model.RoleAssistant, "后答", "https://b.com", nil)

		got, err := st.RecentGlobalRounds(ctx, key, 2)
		if err != nil {
			t.Fatalf("坏行不应导致报错: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("rounds = %d, want 2（坏行跳过、轮次不中断）", len(got))
		}
		if got[1].FinalEvent.Response.Choices[0].Message.Content != "后答" {
			t.Errorf("最新轮应完整: %+v", got[1])
		}
	})

	t.Run("空会话返回空", func(t *testing.T) {
		st := newRoundsTestStore(t)
		_ = mustSession(st)
		got, err := st.RecentGlobalRounds(ctx, key, 2)
		if err != nil {
			t.Fatalf("RecentGlobalRounds: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("rounds = %d, want 0", len(got))
		}
	})

	t.Run("接口断言", func(t *testing.T) {
		var _ storage.Storage = newRoundsTestStore(t)
	})
}
