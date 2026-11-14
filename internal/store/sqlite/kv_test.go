package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"absorb-agent/internal/store/storage"

	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/session"
)

func newKVTestStore(t *testing.T) storage.Storage {
	t.Helper()
	st, err := newSQLite(filepath.Join(t.TempDir(), "kv.db"))
	if err != nil {
		t.Fatalf("newSQLite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestNormalizeScopeID scope 身份 URL 规范化（storage 层）：
// trim/fragment/尾斜杠/scheme+host 小写/utm 参数/query 排序/根路径边界；
// 每个用例同时校验归一幂等。
func TestNormalizeScopeID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"空串返回空", "", ""},
		{"首尾空白 trim", "  https://a.com/p  ", "https://a.com/p"},
		{"非 URL 原样返回", "not a url", "not a url"},
		{"无 scheme 原样返回", "a.com/p", "a.com/p"},
		{"无 host 原样返回", "mailto:A@B.com", "mailto:A@B.com"},
		{"data URI 原样返回", "data:text/plain,hi", "data:text/plain,hi"},
		{"控制字符解析失败原样返回", "https://a.com/a\tb", "https://a.com/a\tb"},
		{"去 fragment", "https://a.com/p#sec", "https://a.com/p"},
		{"去尾斜杠", "https://a.com/p/", "https://a.com/p"},
		{"多个尾斜杠全去掉", "https://a.com/p//", "https://a.com/p"},
		{"纯斜杠路径归一根", "https://a.com///", "https://a.com/"},
		{"scheme host 小写 path 保留大小写", "HTTPS://A.COM/Page", "https://a.com/Page"},
		{"host 带端口", "https://A.COM:8443/p/", "https://a.com:8443/p"},
		{"根路径保留", "https://a.com/", "https://a.com/"},
		{"无 path 补根", "https://a.com", "https://a.com/"},
		{"根路径带 fragment", "https://a.com/#sec", "https://a.com/"},
		{"根路径带 query 统一补根", "https://a.com/?x=1", "https://a.com/?x=1"},
		{"空 query 丢弃问号", "https://a.com/p?", "https://a.com/p"},
		{"去 utm 参数", "https://a.com/p?utm_source=x&utm_medium=y&id=2", "https://a.com/p?id=2"},
		{"UTM 大写 key 同样去除", "https://a.com/p?UTM_SOURCE=x&id=2", "https://a.com/p?id=2"},
		{"query 按 key 排序", "https://a.com/p?b=2&a=1", "https://a.com/p?a=1&b=2"},
		{"非 http scheme 同样归一", "ftp://A.com/p/", "ftp://a.com/p"},
		{"尾斜杠 fragment utm 组合归一", "https://A.com/p/?utm_medium=x&q=1#top", "https://a.com/p?q=1"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := storage.NormalizeScopeID(tt.in)
			if got != tt.want {
				t.Errorf("storage.NormalizeScopeID(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if twice := storage.NormalizeScopeID(got); twice != got {
				t.Errorf("normalizeScopeID 不幂等: %q -> %q -> %q", tt.in, got, twice)
			}
		})
	}
}

// FuzzNormalizeScopeID 随机输入健壮性：不 panic 且归一幂等（二次归一不变）。
func FuzzNormalizeScopeID(f *testing.F) {
	f.Add("")
	f.Add("https://a.com/p#sec")
	f.Add("https://A.COM/p/?utm_source=x&id=1")
	f.Add("not a url")
	f.Add("https://a.com/a\tb")
	f.Fuzz(func(t *testing.T, raw string) {
		once := storage.NormalizeScopeID(raw)
		if twice := storage.NormalizeScopeID(once); twice != once {
			t.Errorf("normalizeScopeID 不幂等: %q -> %q -> %q", raw, once, twice)
		}
	})
}

// TestPageTextScopeIsolation 同会话多页面各存一行互不覆盖；未存 scope 返回空。
func TestPageTextScopeIsolation(t *testing.T) {
	t.Parallel()
	st := newKVTestStore(t)
	ctx := context.Background()
	const app, user, sess = "app", "u1", "s1"

	mustSave := func(scope, title, text string) {
		t.Helper()
		if err := st.SavePageText(ctx, app, user, sess, scope, title, text); err != nil {
			t.Fatalf("save %q: %v", scope, err)
		}
	}
	mustSave("https://a.com/post/1", "标题A", "正文A")
	mustSave("https://B.com/post/2/", "标题B", "正文B")

	if got, err := st.LoadPageText(ctx, app, user, sess, "https://a.com/post/1"); err != nil || got != "正文A" {
		t.Fatalf("load A: got %q err %v", got, err)
	}
	// host 小写 + 去尾斜杠归一后命中 B 的缓存
	if got, err := st.LoadPageText(ctx, app, user, sess, "https://b.com/post/2"); err != nil || got != "正文B" {
		t.Fatalf("load B: got %q err %v", got, err)
	}
	// 未存 scope 返回空（宁缺勿错，不回退其他页面缓存）
	if got, err := st.LoadPageText(ctx, app, user, sess, "https://c.com/post/3"); err != nil || got != "" {
		t.Fatalf("load C: got %q err %v", got, err)
	}
}

// TestPageTextSameScopeOverwrite 同 scope 重复保存覆盖原行，不新增行。
func TestPageTextSameScopeOverwrite(t *testing.T) {
	t.Parallel()
	st := newKVTestStore(t)
	ctx := context.Background()
	const app, user, sess, scope = "app", "u1", "s1", "https://a.com/p"

	if err := st.SavePageText(ctx, app, user, sess, scope, "t1", "第一版"); err != nil {
		t.Fatalf("save v1: %v", err)
	}
	if err := st.SavePageText(ctx, app, user, sess, scope, "t2", "第二版"); err != nil {
		t.Fatalf("save v2: %v", err)
	}
	if got, err := st.LoadPageText(ctx, app, user, sess, scope); err != nil || got != "第二版" {
		t.Fatalf("load: got %q err %v", got, err)
	}
	ss := st.(*sqliteStore)
	var n int
	if err := ss.db.QueryRow(`SELECT COUNT(*) FROM page_texts`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
}

// TestGetSessionSkipsBadJSONRow 坏 JSON 行跳过：
// 主链路 GetSession 遇单行损坏不再报错，降级为缺一条历史；
// recall 辅助链路（loadScopeEvents）保持同语义。
func TestGetSessionSkipsBadJSONRow(t *testing.T) {
	t.Parallel()
	st := newKVTestStore(t)
	ctx := context.Background()
	const app, user, sess = "app", "u1", "badjson"

	key := session.Key{AppName: app, UserID: user, SessionID: sess}
	s, err := st.CreateSession(ctx, key, nil)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	mustAppend := func(content string) {
		t.Helper()
		evt := &event.Event{Response: &model.Response{
			Choices: []model.Choice{{Message: model.Message{Role: "user", Content: content}}},
		}}
		if err := st.AppendEvent(ctx, s, evt); err != nil {
			t.Fatalf("append %q: %v", content, err)
		}
	}
	mustAppend("第一条")
	mustAppend("第二条")
	mustAppend("第三条")

	// 直接写坏中间一行（seq=2）
	ss := st.(*sqliteStore)
	if _, err := ss.db.Exec(
		`UPDATE session_events SET event='{broken json' WHERE app_name=? AND user_id=? AND session_id=? AND seq=2`,
		app, user, sess); err != nil {
		t.Fatalf("corrupt row: %v", err)
	}

	// 主链路：不报错，返回其余 2 条（坏行跳过、好行保留、顺序不变）
	got, err := st.GetSession(ctx, key)
	if err != nil {
		t.Fatalf("GetSession 坏行应跳过而非报错: %v", err)
	}
	if got == nil {
		t.Fatal("GetSession 返回 nil")
	}
	if len(got.Events) != 2 {
		t.Fatalf("events = %d, want 2", len(got.Events))
	}
	if c := got.Events[0].Response.Choices[0].Message.Content; c != "第一条" {
		t.Errorf("events[0] = %q, want 第一条", c)
	}
	if c := got.Events[1].Response.Choices[0].Message.Content; c != "第三条" {
		t.Errorf("events[1] = %q, want 第三条", c)
	}

	// 辅助链路一致性：recall 目录同样 2 条
	entries, err := st.ListEventIndex(ctx, key, "")
	if err != nil {
		t.Fatalf("ListEventIndex: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("index entries = %d, want 2", len(entries))
	}
}
