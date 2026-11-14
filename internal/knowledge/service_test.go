package knowledge

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"trpc.group/trpc-go/trpc-agent-go/session"

	"absorb-agent/internal/store/storage"

	// 注册 sqlite 后端，storage.New("sqlite", …) 可用
	_ "absorb-agent/internal/store/sqlite"
)

// newTestStore 临时文件库（自带建库建表与迁移）。
func newTestStore(t *testing.T) storage.Storage {
	t.Helper()
	st, err := storage.New("sqlite", filepath.Join(t.TempDir(), "knowledge-test.db"))
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// seedSession 预建来源会话：M2.5 起保存链路有 L1 校验，origin session 须真实存在。
func seedSession(t *testing.T, st storage.Storage, app, user, id string) {
	t.Helper()
	if _, err := st.CreateSession(context.Background(), session.Key{AppName: app, UserID: user, SessionID: id}, nil); err != nil {
		t.Fatalf("CreateSession(%s/%s/%s): %v", app, user, id, err)
	}
}

func createInput() CreateInput {
	return CreateInput{
		Title:             "checkpoint 机制",
		AISummary:         "WAL 通过 checkpoint 把日志并回主库",
		UserUnderstanding: "",
		OriginSessionID:   "s1",
		OriginEventID:     "e1",
		Sources: []SourceInput{{
			SourceTitle:     "SQLite WAL 文档",
			SourceURL:       "https://example.invalid/wal",
			QuoteText:       "checkpoint 会把 WAL 中的内容写回数据库",
			SourceSessionID: "s1",
			SourceEventID:   "e1",
		}},
	}
}

// 创建→幂等重复提交→读取：验收标准"同一保存请求重复提交，不会产生重复记录"。
func TestServiceCreateIdempotent(t *testing.T) {
	st := newTestStore(t)
	seedSession(t, st, "app", "user", "s1")
	svc := NewService(st)
	ctx := context.Background()

	item, created, err := svc.Create(ctx, "app", "user", createInput())
	if err != nil || !created {
		t.Fatalf("create: %v, created=%v", err, created)
	}
	if item.Status != storage.KnowledgeStatusActive {
		t.Fatalf("status = %q, want active", item.Status)
	}
	if item.DedupKey != dedupKey("s1", "e1") {
		t.Fatalf("dedup key not derived from origin: %q", item.DedupKey)
	}

	// 同一回答事件重复提交：created=false、同一条目、来源不增
	dup, created, err := svc.Create(ctx, "app", "user", createInput())
	if err != nil || created {
		t.Fatalf("duplicate create: %v, created=%v", err, created)
	}
	if dup.ID != item.ID {
		t.Fatalf("duplicate returned %s, want %s", dup.ID, item.ID)
	}
	_, srcs, err := svc.Get(ctx, "app", "user", item.ID)
	if err != nil || len(srcs) != 1 {
		t.Fatalf("sources after duplicate: %v, n=%d", err, len(srcs))
	}
}

// 入参校验：标题必填、来源 ≥ 1、引用必填、状态枚举合法。
func TestServiceValidation(t *testing.T) {
	st := newTestStore(t)
	seedSession(t, st, "app", "user", "s1") // 保证引用必填校验失败是字段问题而非来源缺失
	svc := NewService(st)
	ctx := context.Background()

	bad := createInput()
	bad.Title = "  "
	if _, _, err := svc.Create(ctx, "app", "user", bad); err == nil {
		t.Fatal("blank title accepted")
	}
	bad = createInput()
	bad.Sources = nil
	if _, _, err := svc.Create(ctx, "app", "user", bad); err == nil {
		t.Fatal("zero sources accepted")
	}
	bad = createInput()
	bad.Sources[0].QuoteText = " "
	if _, _, err := svc.Create(ctx, "app", "user", bad); err == nil {
		t.Fatal("blank quote accepted")
	}
	if _, err := svc.List(ctx, "app", "user", "bogus"); err == nil {
		t.Fatal("invalid list status accepted")
	}
	if _, _, err := svc.Get(ctx, "app", "user", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing: %v, want ErrNotFound", err)
	}
	if err := svc.Update(ctx, "app", "user", "missing", "t", "a", "u"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing: %v, want ErrNotFound", err)
	}
	if err := svc.SetStatus(ctx, "app", "user", "missing", storage.KnowledgeStatusArchived); !errors.Is(err, ErrNotFound) {
		t.Fatalf("set status missing: %v, want ErrNotFound", err)
	}
}

// 编辑不改来源：验收标准"用户可以修改 AI 提炼，但修改不会改变原始引用"。
func TestServiceUpdateKeepsSources(t *testing.T) {
	st := newTestStore(t)
	seedSession(t, st, "app", "user", "s1")
	svc := NewService(st)
	ctx := context.Background()

	item, _, err := svc.Create(ctx, "app", "user", createInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	before, srcsBefore, err := svc.Get(ctx, "app", "user", item.ID)
	if err != nil {
		t.Fatalf("get before: %v", err)
	}

	if err := svc.Update(ctx, "app", "user", item.ID, "新标题", "改成自己的话", "checkpoint 我理解为定期落盘"); err != nil {
		t.Fatalf("update: %v", err)
	}
	after, srcsAfter, err := svc.Get(ctx, "app", "user", item.ID)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if after.Title != "新标题" || after.AISummary != "改成自己的话" || after.UserUnderstanding != "checkpoint 我理解为定期落盘" {
		t.Fatalf("update not applied: %+v", after)
	}
	if len(srcsAfter) != len(srcsBefore) || srcsAfter[0].QuoteText != srcsBefore[0].QuoteText ||
		srcsAfter[0].ID != srcsBefore[0].ID || srcsAfter[0].SourceURL != srcsBefore[0].SourceURL {
		t.Fatalf("sources changed by update: %+v -> %+v", srcsBefore, srcsAfter)
	}
	if before.Status != storage.KnowledgeStatusActive || after.Status != storage.KnowledgeStatusActive {
		t.Fatal("status should be untouched by text update")
	}
}

// 归档与删除：验收标准"用户可以删除知识，或者执行归档操作"。
func TestServiceArchiveAndDelete(t *testing.T) {
	st := newTestStore(t)
	seedSession(t, st, "app", "user", "s1")
	svc := NewService(st)
	ctx := context.Background()

	item, _, err := svc.Create(ctx, "app", "user", createInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := svc.SetStatus(ctx, "app", "user", item.ID, storage.KnowledgeStatusArchived); err != nil {
		t.Fatalf("archive: %v", err)
	}
	active, err := svc.List(ctx, "app", "user", storage.KnowledgeStatusActive)
	if err != nil || len(active) != 0 {
		t.Fatalf("list active after archive: %v, n=%d", err, len(active))
	}
	archived, err := svc.List(ctx, "app", "user", storage.KnowledgeStatusArchived)
	if err != nil || len(archived) != 1 {
		t.Fatalf("list archived: %v, n=%d", err, len(archived))
	}
	all, err := svc.List(ctx, "app", "user", "")
	if err != nil || len(all) != 1 {
		t.Fatalf("list all: %v, n=%d", err, len(all))
	}

	if err := svc.Delete(ctx, "app", "user", item.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, _, err := svc.Get(ctx, "app", "user", item.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete: %v, want ErrNotFound", err)
	}
	// 删除幂等：再删不报错
	if err := svc.Delete(ctx, "app", "user", item.ID); err != nil {
		t.Fatalf("delete again: %v", err)
	}
}

// 用户隔离：不同 user 的知识互不可见、互不冲突（幂等键按 user 隔离）。
func TestServiceUserIsolation(t *testing.T) {
	st := newTestStore(t)
	seedSession(t, st, "app", "user-a", "s1")
	seedSession(t, st, "app", "user-b", "s1")
	svc := NewService(st)
	ctx := context.Background()

	a, created, err := svc.Create(ctx, "app", "user-a", createInput())
	if err != nil || !created {
		t.Fatalf("create user-a: %v, created=%v", err, created)
	}
	b, created, err := svc.Create(ctx, "app", "user-b", createInput())
	if err != nil || !created {
		t.Fatalf("same origin different user should create: %v, created=%v", err, created)
	}
	if a.ID == b.ID {
		t.Fatal("different users should get different items")
	}
	if _, _, err := svc.Get(ctx, "app", "user-a", b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user get: %v, want ErrNotFound", err)
	}
}

// M2.5 conversation 来源不变量：quote 强制清空（防客户端旧逻辑把非原文文本
// 写进证据字段），且带 URL 的来源回填 source_document_id（同 URL 复用同一文档锚点）。
func TestServiceConversationSourceInvariant(t *testing.T) {
	st := newTestStore(t)
	seedSession(t, st, "app", "user", "s1")
	svc := NewService(st)
	ctx := context.Background()

	in := createInput()
	in.Sources[0].SourceType = storage.KnowledgeSourceConversation
	in.Sources[0].QuoteText = "这段是 AI 改写的话术，绝不能伪装成原文"
	item, created, err := svc.Create(ctx, "app", "user", in)
	if err != nil || !created {
		t.Fatalf("create: %v, created=%v", err, created)
	}
	_, srcs, err := svc.Get(ctx, "app", "user", item.ID)
	if err != nil || len(srcs) != 1 {
		t.Fatalf("get sources: %v, n=%d", err, len(srcs))
	}
	if srcs[0].SourceType != storage.KnowledgeSourceConversation {
		t.Fatalf("source type = %q", srcs[0].SourceType)
	}
	if srcs[0].QuoteText != "" {
		t.Fatalf("conversation quote must be forced empty, got %q", srcs[0].QuoteText)
	}
	if srcs[0].SourceDocumentID == "" {
		t.Fatal("source_document_id not backfilled")
	}

	// 反向：quote 来源缺引用仍拒绝
	bad := createInput()
	bad.Sources[0].QuoteText = ""
	if _, _, err := svc.Create(ctx, "app", "user", bad); !errors.Is(err, ErrValidation) {
		t.Fatalf("blank quote on quote source: %v, want ErrValidation", err)
	}
}

// L1 来源校验：origin session 不存在 / 为空 → ErrOriginSession。
func TestServiceCreateRejectsUnknownSession(t *testing.T) {
	st := newTestStore(t)
	seedSession(t, st, "app", "user", "s1")
	svc := NewService(st)
	ctx := context.Background()

	ghost := createInput()
	ghost.OriginSessionID = "ghost"
	if _, _, err := svc.Create(ctx, "app", "user", ghost); !errors.Is(err, ErrOriginSession) {
		t.Fatalf("unknown session: %v, want ErrOriginSession", err)
	}
	empty := createInput()
	empty.OriginSessionID = ""
	if _, _, err := svc.Create(ctx, "app", "user", empty); !errors.Is(err, ErrOriginSession) {
		t.Fatalf("empty session: %v, want ErrOriginSession", err)
	}
}

// SaveReadingSummary 生命周期：校验 → 幂等命中（同回答重复提交）→ 新回答覆盖。
// 摘要存 documents（层次A），与知识条目互不影响。
func TestServiceSaveReadingSummary(t *testing.T) {
	st := newTestStore(t)
	seedSession(t, st, "app", "user", "s1")
	svc := NewService(st)
	ctx := context.Background()

	in := SaveSummaryInput{SessionID: "s1", RequestID: "r1", URL: "https://example.invalid/wal",
		Title: "SQLite WAL", SummaryText: "WAL 把随机写摊平成顺序追加"}
	doc, created, err := svc.SaveReadingSummary(ctx, "app", "user", in)
	if err != nil || !created {
		t.Fatalf("save: %v, created=%v", err, created)
	}
	if doc.ID == "" || doc.SummaryText != in.SummaryText || doc.SummarySessionID != "s1" || doc.SummaryEventID != "r1" {
		t.Fatalf("doc mismatch: %+v", doc)
	}

	// 同一回答重复提交：幂等 created=false，内容不变
	in.Title = "标题变化不应新建锚点"
	dup, created, err := svc.SaveReadingSummary(ctx, "app", "user", in)
	if err != nil || created {
		t.Fatalf("idempotent save: %v, created=%v", err, created)
	}
	if dup.ID != doc.ID {
		t.Fatalf("idempotent doc = %s, want %s", dup.ID, doc.ID)
	}

	// 再次总结（新 RequestID）：合法覆盖，仍同一文档
	in2 := in
	in2.RequestID = "r2"
	in2.SummaryText = "重新总结后的版本"
	over, created, err := svc.SaveReadingSummary(ctx, "app", "user", in2)
	if err != nil || !created {
		t.Fatalf("overwrite save: %v, created=%v", err, created)
	}
	if over.ID != doc.ID || over.SummaryText != in2.SummaryText {
		t.Fatalf("overwrite mismatch: %+v", over)
	}

	// 校验：缺必填 → ErrValidation；未知会话 → ErrOriginSession
	noURL := in
	noURL.URL = " "
	if _, _, err := svc.SaveReadingSummary(ctx, "app", "user", noURL); !errors.Is(err, ErrValidation) {
		t.Fatalf("missing url: %v, want ErrValidation", err)
	}
	noSess := in
	noSess.SessionID = "ghost"
	if _, _, err := svc.SaveReadingSummary(ctx, "app", "user", noSess); !errors.Is(err, ErrOriginSession) {
		t.Fatalf("unknown session: %v, want ErrOriginSession", err)
	}
}
