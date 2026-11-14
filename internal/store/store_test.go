package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/session"

	// 测试中直接打开底层 DB 构造旧 schema，验证 sqlite 后端的升级迁移
	_ "modernc.org/sqlite"
)

// newTestService 用临时目录的 db 文件建一个会话服务，返回 (svc, dbPath)。
// 调用方负责 svc.Close()（ReopenRecovery 用例需中途关闭后重开）。
func newTestService(t *testing.T) (session.Service, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	svc, err := NewSessionService(dbPath)
	if err != nil {
		t.Fatalf("NewSessionService: %v", err)
	}
	return svc, dbPath
}

// mkEvent 构造一条带 Response 的事件（模拟一轮 user/assistant 消息）。
func mkEvent(role, content string) *event.Event {
	finish := "stop"
	return &event.Event{
		Response: &model.Response{
			ID:     "resp",
			Object: "chat.completion",
			Model:  "test",
			Choices: []model.Choice{{
				Index: 0,
				Message: model.Message{
					Role:    model.Role(role),
					Content: content,
				},
				FinishReason: &finish,
			}},
		},
	}
}

func TestSQLiteCreateGetAppend(t *testing.T) {
	svc, _ := newTestService(t)
	defer svc.Close()
	ctx := context.Background()
	key := session.Key{AppName: "absorb", UserID: "u1", SessionID: "s1"}

	if _, err := svc.CreateSession(ctx, key, nil); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sess, err := svc.GetSession(ctx, key)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess == nil {
		t.Fatalf("GetSession 返回 nil（CreateSession 后应存在）")
	}
	if err := svc.AppendEvent(ctx, sess, mkEvent("user", "区块链是什么？")); err != nil {
		t.Fatalf("AppendEvent 1: %v", err)
	}
	if err := svc.AppendEvent(ctx, sess, mkEvent("assistant", "区块链是分布式账本技术。")); err != nil {
		t.Fatalf("AppendEvent 2: %v", err)
	}

	got, err := svc.GetSession(ctx, key)
	if err != nil {
		t.Fatalf("GetSession after append: %v", err)
	}
	if len(got.Events) != 2 {
		t.Fatalf("events len = %d, want 2", len(got.Events))
	}
}

func TestSQLiteReopenRecovery(t *testing.T) {
	svc, dbPath := newTestService(t)
	ctx := context.Background()
	key := session.Key{AppName: "absorb", UserID: "u1", SessionID: "s2"}

	svc.CreateSession(ctx, key, nil)
	sess, _ := svc.GetSession(ctx, key)
	svc.AppendEvent(ctx, sess, mkEvent("user", "第一轮提问"))
	svc.AppendEvent(ctx, sess, mkEvent("assistant", "第一轮回答"))

	// 模拟服务重启：关闭旧实例，用同 dbPath 新建
	if err := svc.Close(); err != nil {
		t.Fatalf("close old: %v", err)
	}
	svc2, err := NewSessionService(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer svc2.Close()

	got, err := svc2.GetSession(ctx, key)
	if err != nil {
		t.Fatalf("GetSession after reopen: %v", err)
	}
	if got == nil || len(got.Events) != 2 {
		t.Fatalf("after reopen events len = %d, want 2（重启应恢复历史）", len(got.Events))
	}
}

func TestSQLiteDeleteSession(t *testing.T) {
	svc, _ := newTestService(t)
	defer svc.Close()
	ctx := context.Background()
	key := session.Key{AppName: "absorb", UserID: "u1", SessionID: "s3"}

	svc.CreateSession(ctx, key, nil)
	sess, _ := svc.GetSession(ctx, key)
	svc.AppendEvent(ctx, sess, mkEvent("user", "q"))

	if err := svc.DeleteSession(ctx, key); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	got, err := svc.GetSession(ctx, key)
	if err != nil {
		t.Fatalf("GetSession after delete: %v", err)
	}
	if got != nil {
		t.Fatalf("GetSession after delete 非 nil，期望 nil")
	}
}

func TestSQLiteAppendIncremental(t *testing.T) {
	svc, _ := newTestService(t)
	defer svc.Close()
	ctx := context.Background()
	key := session.Key{AppName: "absorb", UserID: "u1", SessionID: "s4"}

	svc.CreateSession(ctx, key, nil)
	sess, _ := svc.GetSession(ctx, key)
	const n = 5
	for i := range n {
		if err := svc.AppendEvent(ctx, sess, mkEvent("user", "q"+strconv.Itoa(i))); err != nil {
			t.Fatalf("AppendEvent %d: %v", i, err)
		}
	}
	got, _ := svc.GetSession(ctx, key)
	if len(got.Events) != n {
		t.Fatalf("events len = %d, want %d", len(got.Events), n)
	}
}

// TestSQLiteSpecialCharsEvents 验证含特殊字符/大文本的 event 落库后读回内容完全一致。
// 覆盖：超长中文、双引号+反斜杠、换行/制表/回车、emoji+生僻字+扩展 unicode、
// 三引号（与提示词分隔符冲突的字符）、中英标点混合、JSON 转义序列本身。
// 这类内容在 SQL 写入 + JSON 序列化两层都易出问题，需端到端验证。
func TestSQLiteSpecialCharsEvents(t *testing.T) {
	svc, _ := newTestService(t)
	defer svc.Close()
	ctx := context.Background()
	key := session.Key{AppName: "absorb", UserID: "u-sp", SessionID: "s-special"}

	if _, err := svc.CreateSession(ctx, key, nil); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sess, err := svc.GetSession(ctx, key)
	if err != nil || sess == nil {
		t.Fatalf("GetSession: err=%v sess=%v", err, sess)
	}

	cases := []string{
		// 1. 超长中文文本（约 5KB）
		strings.Repeat("区块链是一种分布式账本技术，数据不可篡改。", 100),
		// 2. 双引号 + 反斜杠（JSON/SQL 双重转义点）
		`她说："C:\Users\Admin\" 是路径`,
		// 3. 换行/制表/回车（控制字符，JSON 标准要求转义）
		"第一行\n第二行\t缩进\r\nWindows换行",
		// 4. emoji + 生僻字 + 扩展 unicode（多字节码点）
		"😀emoji 表情 𠀋生僻字 ☑︎选择符",
		// 5. 三引号（与提示词分隔符 """ 冲突，验证不破坏存储）
		`代码片段 """ 包裹的边界符`,
		// 6. 中英标点混合
		"中English混合，。,.！!？?「」\"\"",
		// 7. JSON 转义序列字面量（验证不会被二次转义）
		`字面序列 \n \t \r \" \\ 文本`,
	}

	// 用例名与 cases 一一对应。cases 内含大量特殊字符字面量，
	// 故名称单独维护（并校验数量一致），避免改写字面量时引入失真。
	names := []string{
		"超长中文", "双引号反斜杠", "控制字符-换行制表回车", "emoji生僻字扩展unicode",
		"三引号边界符", "中英标点混合", "JSON转义序列字面量",
	}
	if len(names) != len(cases) {
		t.Fatalf("用例名数量 %d 与 cases %d 不一致", len(names), len(cases))
	}

	// 写入
	for i, content := range cases {
		if err := svc.AppendEvent(ctx, sess, mkEvent("user", content)); err != nil {
			t.Fatalf("AppendEvent %s(%d): %v", names[i], i, err)
		}
	}

	// 读回并逐条断言
	got, err := svc.GetSession(ctx, key)
	if err != nil {
		t.Fatalf("GetSession after append: %v", err)
	}
	if len(got.Events) != len(cases) {
		t.Fatalf("events len = %d, want %d", len(got.Events), len(cases))
	}
	for i, want := range cases {
		t.Run(names[i], func(t *testing.T) {
			gotContent := got.Events[i].Response.Choices[0].Message.Content
			if gotContent != want {
				t.Errorf("event[%d] content mismatch\nwant: %q\n got: %q", i, want, gotContent)
			}
		})
	}
}

// TestUpgradeModelConfigsLegacyThinking 旧库升级：bool thinking（0/1）迁移为三态（0→2、1→1），新列补齐。
func TestUpgradeModelConfigsLegacyThinking(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	svc, err := NewSessionService(dbPath)
	if err != nil {
		t.Fatalf("创建库失败: %v", err)
	}
	ctx := context.Background()
	// 回退成旧结构：无 tool_rounds/image_support，thinking 为 bool 语义。
	// 直接打开底层 DB 构造旧 schema：db 已下沉到 sqlite 后端，不再经 SessionService 暴露。
	legacyDB, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacyDB.ExecContext(ctx, `DROP TABLE model_configs`); err != nil {
		t.Fatalf("DROP: %v", err)
	}
	if _, err := legacyDB.ExecContext(ctx, `CREATE TABLE model_configs (
		id TEXT PRIMARY KEY, provider TEXT NOT NULL DEFAULT '', display_name TEXT NOT NULL DEFAULT '',
		model_id TEXT NOT NULL, api_format TEXT NOT NULL DEFAULT 'openai_chat', base_url TEXT NOT NULL,
		full_url INTEGER NOT NULL DEFAULT 0, api_key TEXT NOT NULL DEFAULT '',
		temperature REAL, max_tokens INTEGER, thinking INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`); err != nil {
		t.Fatalf("CREATE 旧表: %v", err)
	}
	now := time.Now().Unix()
	for _, row := range [][]any{{"on", 1}, {"off", 0}} {
		if _, err := legacyDB.ExecContext(ctx,
			`INSERT INTO model_configs(id, provider, model_id, api_format, base_url, thinking, created_at, updated_at)
			 VALUES(?, 'bigmodel', 'm', 'openai_chat', 'https://x', ?, ?, ?)`, row[0], row[1], now, now); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
	}
	// 同时清空迁移账本：模拟"迁移引擎上线前"创建的旧库（旧结构 + 无 schema_migrations 记录）。
	// 只回退表结构而保留账本的话，账本会判定 v1 已应用、不再跑迁移——那不是真实旧库形态。
	if _, err := legacyDB.ExecContext(ctx, `DELETE FROM schema_migrations`); err != nil {
		t.Fatalf("清账本: %v", err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}
	_ = svc.Close()

	// 重新打开触发升级
	svc2, err := NewSessionService(dbPath)
	if err != nil {
		t.Fatalf("重开库失败: %v", err)
	}
	t.Cleanup(func() { _ = svc2.Close() })
	on, _ := svc2.GetModelConfig(ctx, "on")
	off, _ := svc2.GetModelConfig(ctx, "off")
	if on == nil || off == nil {
		t.Fatalf("升级后行丢失: %v %v", on, off)
	}
	if on.Thinking != 1 || off.Thinking != 2 {
		t.Fatalf("bool→三态迁移不符: on=%d off=%d", on.Thinking, off.Thinking)
	}
	if !on.ImageSupport || on.ToolRounds != nil {
		t.Fatalf("新列默认值不符: img=%v rounds=%v", on.ImageSupport, on.ToolRounds)
	}
}
