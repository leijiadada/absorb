package sqlite

import (
	"context"
	"testing"

	"absorb-agent/internal/store/storage"
)

// knowledgeFixture 构造一条测试知识（2 条来源）。
func knowledgeFixture(id, dedup string) (*storage.KnowledgeItem, []*storage.KnowledgeSource) {
	item := &storage.KnowledgeItem{
		ID:                id,
		AppName:           "app",
		UserID:            "user",
		Title:             "checkpoint 机制",
		AISummary:         "WAL 通过 checkpoint 把日志并回主库",
		UserUnderstanding: "",
		Status:            storage.KnowledgeStatusActive,
		OriginSessionID:   "s1",
		OriginEventID:     "e1",
		DedupKey:          dedup,
	}
	sources := []*storage.KnowledgeSource{
		{
			ID:              id + "-src1",
			ItemID:          id,
			AppName:         "app",
			UserID:          "user",
			SourceType:      "quote",
			SourceTitle:     "SQLite WAL 文档",
			SourceURL:       "https://example.invalid/wal",
			QuoteText:       "checkpoint 会把 WAL 中的内容写回数据库",
			SourceSessionID: "s1",
			SourceEventID:   "e1",
		},
		{
			ID:              id + "-src2",
			ItemID:          id,
			AppName:         "app",
			UserID:          "user",
			SourceType:      "quote",
			SourceTitle:     "SQLite WAL 文档",
			SourceURL:       "https://example.invalid/wal",
			QuoteText:       "默认每 1000 页自动执行一次 checkpoint",
			SourceSessionID: "s1",
			SourceEventID:   "e1",
		},
	}
	return item, sources
}

// 存储层 CRUD 全链路：创建→读取（含来源）→编辑（来源不动）→归档→删除（来源同清）。
func TestKnowledgeStorageRoundtrip(t *testing.T) {
	st, _ := openStore(t, "knowledge-crud.db")
	ctx := context.Background()

	item, sources := knowledgeFixture("ki-1", "dk-1")
	got, created, err := st.CreateKnowledge(ctx, "app", "user", item, sources)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !created || got.ID != "ki-1" {
		t.Fatalf("create = (%v, %s), want (true, ki-1)", created, got.ID)
	}
	// 创建返回值同样须携带库内时间戳（前端拿到的快照与库内一致）
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("create return missing timestamps: %+v", got)
	}

	it, srcs, err := st.GetKnowledge(ctx, "app", "user", "ki-1")
	if err != nil || it == nil {
		t.Fatalf("get: %v, item=%v", err, it)
	}
	if it.Title != item.Title || it.AISummary != item.AISummary || it.Status != storage.KnowledgeStatusActive ||
		it.OriginSessionID != "s1" || it.OriginEventID != "e1" || it.DedupKey != "dk-1" || it.AppName != "app" {
		t.Fatalf("item roundtrip mismatch: %+v", it)
	}
	if it.CreatedAt.IsZero() || it.UpdatedAt.IsZero() {
		t.Fatalf("timestamps not filled: %+v", it)
	}
	if len(srcs) != 2 {
		t.Fatalf("sources = %d, want 2", len(srcs))
	}
	if srcs[0].QuoteText != sources[0].QuoteText || srcs[0].SourceURL != sources[0].SourceURL ||
		srcs[0].ItemID != "ki-1" || srcs[0].CreatedAt.IsZero() {
		t.Fatalf("source roundtrip mismatch: %+v", srcs[0])
	}

	// 列表：全部 / 按 status 过滤
	all, err := st.ListKnowledge(ctx, "app", "user", "")
	if err != nil || len(all) != 1 {
		t.Fatalf("list all: %v, n=%d", err, len(all))
	}
	if all[0].ID != "ki-1" {
		t.Fatalf("list all id = %s", all[0].ID)
	}
	none, err := st.ListKnowledge(ctx, "app", "user", storage.KnowledgeStatusArchived)
	if err != nil || len(none) != 0 {
		t.Fatalf("list archived before archive: %v, n=%d", err, len(none))
	}

	// 编辑可变文本：三个文本列变化，来源原样
	if err := st.UpdateKnowledgeText(ctx, "app", "user", "ki-1", "新标题", "新提炼", "我的理解"); err != nil {
		t.Fatalf("update text: %v", err)
	}
	it2, srcs2, err := st.GetKnowledge(ctx, "app", "user", "ki-1")
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if it2.Title != "新标题" || it2.AISummary != "新提炼" || it2.UserUnderstanding != "我的理解" {
		t.Fatalf("update not applied: %+v", it2)
	}
	if len(srcs2) != 2 || srcs2[0].QuoteText != sources[0].QuoteText || srcs2[1].QuoteText != sources[1].QuoteText {
		t.Fatalf("sources changed by text update: %+v", srcs2)
	}

	// 归档 / 重开
	if err := st.SetKnowledgeStatus(ctx, "app", "user", "ki-1", storage.KnowledgeStatusArchived); err != nil {
		t.Fatalf("archive: %v", err)
	}
	archived, err := st.ListKnowledge(ctx, "app", "user", storage.KnowledgeStatusArchived)
	if err != nil || len(archived) != 1 || archived[0].ID != "ki-1" {
		t.Fatalf("list archived: %v, %v", err, archived)
	}
	active, err := st.ListKnowledge(ctx, "app", "user", storage.KnowledgeStatusActive)
	if err != nil || len(active) != 0 {
		t.Fatalf("list active after archive: %v, n=%d", err, len(active))
	}

	// 硬删除：条目与来源同清，且幂等
	if err := st.DeleteKnowledge(ctx, "app", "user", "ki-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := st.DeleteKnowledge(ctx, "app", "user", "ki-1"); err != nil {
		t.Fatalf("delete again: %v", err)
	}
	it3, srcs3, err := st.GetKnowledge(ctx, "app", "user", "ki-1")
	if err != nil || it3 != nil || srcs3 != nil {
		t.Fatalf("get after delete: %v, %v, %v", err, it3, srcs3)
	}
}

// 幂等：同 (app,user,dedup_key) 重复创建不新增、不触碰既有 sources，回读既有条目。
func TestKnowledgeStorageCreateIdempotent(t *testing.T) {
	st, _ := openStore(t, "knowledge-idem.db")
	ctx := context.Background()

	item1, sources1 := knowledgeFixture("ki-1", "dk-same")
	if _, created, err := st.CreateKnowledge(ctx, "app", "user", item1, sources1); err != nil || !created {
		t.Fatalf("first create: %v, created=%v", err, created)
	}

	// 第二次提交：不同 id、不同 sources，但同一 dedup_key
	item2, sources2 := knowledgeFixture("ki-2", "dk-same")
	got, created, err := st.CreateKnowledge(ctx, "app", "user", item2, sources2)
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if created {
		t.Fatal("second create reported created=true, want false")
	}
	if got.ID != "ki-1" {
		t.Fatalf("idempotent create returned %s, want existing ki-1", got.ID)
	}

	// 既有 sources 未被第二次提交污染（仍为第一份的 2 条）
	_, srcs, err := st.GetKnowledge(ctx, "app", "user", "ki-1")
	if err != nil || len(srcs) != 2 || srcs[0].ID != "ki-1-src1" {
		t.Fatalf("sources after duplicate create: %v, %v", err, srcs)
	}
	// ki-2 未被创建
	it2, _, err := st.GetKnowledge(ctx, "app", "user", "ki-2")
	if err != nil || it2 != nil {
		t.Fatalf("ki-2 should not exist: %v, %v", err, it2)
	}
}

// 用户隔离：幂等键按 (app,user) 隔离——同 dedup 不同 user 各自成条；
// 但 id 全局唯一（与 model_configs 的 TEXT PRIMARY KEY 一致），跨用户须用不同 id。
func TestKnowledgeStorageUserIsolation(t *testing.T) {
	st, _ := openStore(t, "knowledge-iso.db")
	ctx := context.Background()

	item, sources := knowledgeFixture("ki-1", "dk-1")
	if _, _, err := st.CreateKnowledge(ctx, "app", "user-a", item, sources); err != nil {
		t.Fatalf("create: %v", err)
	}
	it, _, err := st.GetKnowledge(ctx, "app", "user-b", "ki-1")
	if err != nil || it != nil {
		t.Fatalf("cross-user read should be empty: %v, %v", err, it)
	}
	// 同 dedup 不同 user：允许各自创建（幂等键含 user_id）
	itemB, sourcesB := knowledgeFixture("ki-2", "dk-1")
	if _, created, err := st.CreateKnowledge(ctx, "app", "user-b", itemB, sourcesB); err != nil || !created {
		t.Fatalf("same dedup different user should be allowed: %v, created=%v", err, created)
	}
	// 同 user 同 dedup 仍被幂等拦截
	itemDup, sourcesDup := knowledgeFixture("ki-3", "dk-1")
	dup, created, err := st.CreateKnowledge(ctx, "app", "user-b", itemDup, sourcesDup)
	if err != nil || created || dup.ID != "ki-2" {
		t.Fatalf("same user same dedup should be idempotent: %v, created=%v, id=%s", err, created, dup.ID)
	}
}
