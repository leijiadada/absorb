package sqlite

// scope_urls 映射表测试：upsert 幂等刷新、查询、列表、会话隔离。

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/session"

	"absorb-agent/internal/store/storage"
)

func newScopeTestStore(t *testing.T) storage.Storage {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "scope.db")
	st, err := storage.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.InitSchema(context.Background()); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}
	return st
}

func TestScopeURLMapping(t *testing.T) {
	st := newScopeTestStore(t)
	ctx := context.Background()
	key := session.Key{AppName: "absorb", UserID: "u1", SessionID: "s-scope"}
	if _, err := st.CreateSession(ctx, key, nil); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	idA := storage.ScopeID("https://a.example.com/article")
	idB := storage.ScopeID("https://b.example.com/article")

	// 空标识/空网址不写库（无害输入）
	if err := st.UpsertScopeURL(ctx, key.AppName, key.UserID, key.SessionID, "", "https://x.com"); err != nil {
		t.Fatalf("UpsertScopeURL(empty id): %v", err)
	}
	if err := st.UpsertScopeURL(ctx, key.AppName, key.UserID, key.SessionID, idA, "  "); err != nil {
		t.Fatalf("UpsertScopeURL(empty url): %v", err)
	}
	if urls, err := st.ListScopeURLs(ctx, key.AppName, key.UserID, key.SessionID); err != nil || len(urls) != 0 {
		t.Fatalf("空输入不应留下映射：urls=%v err=%v", urls, err)
	}

	// 两个 scope 登记
	if err := st.UpsertScopeURL(ctx, key.AppName, key.UserID, key.SessionID, idA, "https://a.example.com/article"); err != nil {
		t.Fatalf("UpsertScopeURL(A): %v", err)
	}
	if err := st.UpsertScopeURL(ctx, key.AppName, key.UserID, key.SessionID, idB, "https://b.example.com/article"); err != nil {
		t.Fatalf("UpsertScopeURL(B): %v", err)
	}

	got, err := st.ScopeURL(ctx, key.AppName, key.UserID, key.SessionID, idA)
	if err != nil || got != "https://a.example.com/article" {
		t.Fatalf("ScopeURL(A)=%q err=%v", got, err)
	}
	// 未登记标识返回空（不报错）
	if got, err := st.ScopeURL(ctx, key.AppName, key.UserID, key.SessionID, "0123456789abcdef"); err != nil || got != "" {
		t.Fatalf("ScopeURL(unknown)=%q err=%v, want 空", got, err)
	}
	urls, err := st.ListScopeURLs(ctx, key.AppName, key.UserID, key.SessionID)
	if err != nil {
		t.Fatalf("ListScopeURLs: %v", err)
	}
	if len(urls) != 2 || urls[idA] != "https://a.example.com/article" || urls[idB] != "https://b.example.com/article" {
		t.Fatalf("ListScopeURLs=%v, want 两条登记", urls)
	}

	// upsert 幂等：重复登记同一标识不增行，网址可刷新
	if err := st.UpsertScopeURL(ctx, key.AppName, key.UserID, key.SessionID, idA, "https://a.example.com/article?v=2"); err != nil {
		t.Fatalf("UpsertScopeURL(A again): %v", err)
	}
	urls, err = st.ListScopeURLs(ctx, key.AppName, key.UserID, key.SessionID)
	if err != nil || len(urls) != 2 {
		t.Fatalf("重复 upsert 后行数应仍为 2：%v err=%v", urls, err)
	}
	if urls[idA] != "https://a.example.com/article?v=2" {
		t.Fatalf("重复 upsert 应刷新网址：%q", urls[idA])
	}

	// 会话隔离：另一会话查不到本会话映射
	other := session.Key{AppName: "absorb", UserID: "u1", SessionID: "s-other"}
	if _, err := st.CreateSession(ctx, other, nil); err != nil {
		t.Fatalf("CreateSession(other): %v", err)
	}
	urls, err = st.ListScopeURLs(ctx, other.AppName, other.UserID, other.SessionID)
	if err != nil || len(urls) != 0 {
		t.Fatalf("跨会话不应可见映射：%v err=%v", urls, err)
	}
	_ = time.Now()
}
