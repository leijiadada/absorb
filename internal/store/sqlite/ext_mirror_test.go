package sqlite

// ext_session_mirror（0005）：镜像幂等 upsert（created_at 首见保留）、索引排序、
// 单条读取与 (app,user) 范围隔离。

import (
	"context"
	"testing"
	"time"
)

func TestExtMirrorUpsertIdempotent(t *testing.T) {
	st, _ := openStore(t, "extmirror.db")
	ctx := context.Background()
	created := time.Unix(1730000000, 0)
	updated := time.Unix(1730000100, 0)

	if err := st.UpsertExtMirror(ctx, "app", "user", "s1", "第一条提问", 2, `{"id":"s1"}`, created, updated); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// 同键重复上传（流式后定稿再传一次）：created_at 保留首见，其余列刷新
	if err := st.UpsertExtMirror(ctx, "app", "user", "s1", "第一条提问（更新）", 4, `{"id":"s1","v":2}`, created, time.Unix(1730000200, 0)); err != nil {
		t.Fatalf("upsert again: %v", err)
	}
	rec, err := st.GetExtMirror(ctx, "app", "user", "s1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if rec == nil {
		t.Fatal("get: not found")
	}
	if rec.CreatedAt != created {
		t.Fatalf("created_at = %v, want first-seen %v", rec.CreatedAt, created)
	}
	if rec.UpdatedAt != time.Unix(1730000200, 0) {
		t.Fatalf("updated_at = %v, want refreshed", rec.UpdatedAt)
	}
	if rec.Data != `{"id":"s1","v":2}` || rec.MsgCount != 4 {
		t.Fatalf("record mismatch: %+v", rec)
	}
}

func TestExtMirrorListOrderAndIsolation(t *testing.T) {
	st, _ := openStore(t, "extmirror-list.db")
	ctx := context.Background()

	// 三个会话，updated_at 交错写入；另一 user 一条（范围隔离）
	write := func(user, sid string, updated time.Time) {
		if err := st.UpsertExtMirror(ctx, "app", user, sid, "t-"+sid, 1, `{}`, time.Unix(1730000000, 0), updated); err != nil {
			t.Fatalf("upsert %s: %v", sid, err)
		}
	}
	write("user", "s-old", time.Unix(1730000100, 0))
	write("user", "s-new", time.Unix(1730000300, 0))
	write("user", "s-mid", time.Unix(1730000200, 0))
	write("user-b", "s-other", time.Unix(1730000900, 0))

	items, err := st.ListExtMirrors(ctx, "app", "user")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("list len = %d, want 3", len(items))
	}
	wantOrder := []string{"s-new", "s-mid", "s-old"}
	for i, w := range wantOrder {
		if items[i].SessionID != w {
			t.Fatalf("items[%d] = %s, want %s", i, items[i].SessionID, w)
		}
	}

	other, err := st.ListExtMirrors(ctx, "app", "user-b")
	if err != nil {
		t.Fatalf("list other: %v", err)
	}
	if len(other) != 1 || other[0].SessionID != "s-other" {
		t.Fatalf("isolation broken: %+v", other)
	}
}

func TestExtMirrorGetNotFound(t *testing.T) {
	st, _ := openStore(t, "extmirror-miss.db")
	rec, err := st.GetExtMirror(context.Background(), "app", "user", "no-such")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if rec != nil {
		t.Fatalf("get missing = %+v, want nil", rec)
	}
}
