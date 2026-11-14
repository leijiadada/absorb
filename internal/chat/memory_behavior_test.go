package chat

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
	"trpc.group/trpc-go/trpc-agent-go/memory"
	"trpc.group/trpc-go/trpc-agent-go/memory/extractor"
	memorystore "trpc.group/trpc-go/trpc-agent-go/memory/sqlite"
)

// newTestMemoryService 构建临时记忆库后端（每测试独立 tempdir，结束后自动清理）。
func newTestMemoryService(t *testing.T, svcOpts ...memorystore.ServiceOpt) *memorystore.Service {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "memory.db")
	db, err := sql.Open("sqlite",
		dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open memory db: %v", err)
	}
	svc, err := memorystore.NewService(db, svcOpts...)
	if err != nil {
		_ = db.Close()
		t.Fatalf("new memory service: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	return svc
}

// TestMemoryMultiUserIsolation 不同 userID 的记忆相互隔离。
func TestMemoryMultiUserIsolation(t *testing.T) {
	svc := newTestMemoryService(t)
	ctx := context.Background()

	ua := memory.UserKey{AppName: AppName, UserID: "user-a"}
	ub := memory.UserKey{AppName: AppName, UserID: "user-b"}

	if err := svc.AddMemory(ctx, ua, "A 的记忆一", nil); err != nil {
		t.Fatalf("AddMemory a1: %v", err)
	}
	if err := svc.AddMemory(ctx, ua, "A 的记忆二", nil); err != nil {
		t.Fatalf("AddMemory a2: %v", err)
	}
	if err := svc.AddMemory(ctx, ub, "B 的记忆", nil); err != nil {
		t.Fatalf("AddMemory b1: %v", err)
	}

	if es, err := svc.ReadMemories(ctx, ua, 10); err != nil || len(es) != 2 {
		t.Fatalf("user-a memories = %d, err=%v; want 2", len(es), err)
	}
	if es, err := svc.ReadMemories(ctx, ub, 10); err != nil || len(es) != 1 {
		t.Fatalf("user-b memories = %d, err=%v; want 1", len(es), err)
	}
}

// TestMemoryIdempotentAcrossTopics 同内容不同 topics 重复写不产生重复条目
// （MemoryID 身份不含 topics）。
func TestMemoryIdempotentAcrossTopics(t *testing.T) {
	svc := newTestMemoryService(t)
	ctx := context.Background()
	uk := memory.UserKey{AppName: AppName, UserID: "u1"}

	if err := svc.AddMemory(ctx, uk, "用户是后端工程师", []string{"职业"}); err != nil {
		t.Fatalf("AddMemory 1: %v", err)
	}
	if err := svc.AddMemory(ctx, uk, "用户是后端工程师", []string{"职业", "技术栈"}); err != nil {
		t.Fatalf("AddMemory 2: %v", err)
	}
	es, err := svc.ReadMemories(ctx, uk, 10)
	if err != nil {
		t.Fatalf("ReadMemories: %v", err)
	}
	if len(es) != 1 {
		t.Fatalf("entries = %d, want 1（同内容重复写应幂等）", len(es))
	}
}

// TestMemoryUpdateAndSoftDeleteReactivate 更新与软删除重激活链路。
func TestMemoryUpdateAndSoftDeleteReactivate(t *testing.T) {
	svc := newTestMemoryService(t, memorystore.WithSoftDelete(true))
	ctx := context.Background()
	uk := memory.UserKey{AppName: AppName, UserID: "u1"}

	if err := svc.AddMemory(ctx, uk, "旧表述：用户喜欢长回答", nil); err != nil {
		t.Fatalf("AddMemory: %v", err)
	}
	es, err := svc.ReadMemories(ctx, uk, 10)
	if err != nil || len(es) != 1 {
		t.Fatalf("pre-update entries = %d, err=%v; want 1", len(es), err)
	}

	// 更新为新表述（UpdateMemory 内容变更 → 生成新 MemoryID，旧行留墓碑）
	mk := memory.Key{AppName: uk.AppName, UserID: uk.UserID, MemoryID: es[0].ID}
	if err := svc.UpdateMemory(ctx, mk, "新表述：用户偏好简短回答", nil); err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}
	es, err = svc.ReadMemories(ctx, uk, 10)
	if err != nil || len(es) != 1 || es[0].Memory.Memory != "新表述：用户偏好简短回答" {
		t.Fatalf("after update: n=%d, err=%v, content=%q", len(es), err, es[0].Memory.Memory)
	}

	// 删除新 ID（软删除 → 不可见）
	mk = memory.Key{AppName: uk.AppName, UserID: uk.UserID, MemoryID: es[0].ID}
	if err := svc.DeleteMemory(ctx, mk); err != nil {
		t.Fatalf("DeleteMemory: %v", err)
	}
	if es, err = svc.ReadMemories(ctx, uk, 10); err != nil || len(es) != 0 {
		t.Fatalf("after delete: n=%d, err=%v; want 0", len(es), err)
	}

	// 再写同内容 → 命中墓碑重新激活
	if err := svc.AddMemory(ctx, uk, "新表述：用户偏好简短回答", nil); err != nil {
		t.Fatalf("AddMemory reactivate: %v", err)
	}
	if es, err = svc.ReadMemories(ctx, uk, 10); err != nil || len(es) != 1 {
		t.Fatalf("after reactivate: n=%d, err=%v; want 1", len(es), err)
	}
}

// TestMemoryAutoModeToolExposure auto 模式下写操作工具不暴露给 Agent，检索工具保持可用。
func TestMemoryAutoModeToolExposure(t *testing.T) {
	svc := newTestMemoryService(t,
		memorystore.WithExtractor(extractor.NewExtractor(nil)))
	writeTools := map[string]bool{
		memory.AddToolName: true, memory.UpdateToolName: true,
		memory.DeleteToolName: true, memory.ClearToolName: true,
	}
	got := map[string]bool{}
	for _, tl := range svc.Tools() {
		got[tl.Declaration().Name] = true
	}
	for name := range writeTools {
		if got[name] {
			t.Errorf("auto 模式不应暴露写工具 %s，实际暴露: %v", name, got)
		}
	}
	if !got[memory.SearchToolName] && !got[memory.LoadToolName] {
		t.Errorf("auto 模式应至少暴露 search/load 之一，实际: %v", got)
	}
}

// TestMemorySearchRelevanceTop1 检索相关性：命中目标记忆排首位。
func TestMemorySearchRelevanceTop1(t *testing.T) {
	svc := newTestMemoryService(t)
	ctx := context.Background()
	uk := memory.UserKey{AppName: AppName, UserID: "u1"}

	if err := svc.AddMemory(ctx, uk, "goroutine 是 Go 的轻量级协程，由调度器管理", []string{"Go"}); err != nil {
		t.Fatalf("AddMemory 1: %v", err)
	}
	if err := svc.AddMemory(ctx, uk, "Redis 提供 RDB 与 AOF 两种持久化方式", []string{"Redis"}); err != nil {
		t.Fatalf("AddMemory 2: %v", err)
	}
	hits, err := svc.SearchMemories(ctx, uk, "goroutine 调度")
	if err != nil {
		t.Fatalf("SearchMemories: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("SearchMemories no hits")
	}
	if !strings.Contains(hits[0].Memory.Memory, "goroutine") {
		t.Fatalf("top1 = %q, want goroutine 相关", hits[0].Memory.Memory)
	}
}

// TestMemoryLimitEnforced 每用户记忆条数上限。
func TestMemoryLimitEnforced(t *testing.T) {
	svc := newTestMemoryService(t, memorystore.WithMemoryLimit(2))
	ctx := context.Background()
	uk := memory.UserKey{AppName: AppName, UserID: "u1"}

	for i, c := range []string{"记忆甲", "记忆乙", "记忆丙"} {
		err := svc.AddMemory(ctx, uk, c, nil)
		if i < 2 && err != nil {
			t.Fatalf("AddMemory #%d: %v", i, err)
		}
		if i >= 2 && err == nil {
			t.Fatalf("AddMemory #%d 应因达到上限被拒绝", i)
		}
	}
	es, err := svc.ReadMemories(ctx, uk, 10)
	if err != nil {
		t.Fatalf("ReadMemories: %v", err)
	}
	if len(es) != 2 {
		t.Fatalf("entries = %d, want 2（达到上限后拒绝写入，不淘汰）", len(es))
	}
}
