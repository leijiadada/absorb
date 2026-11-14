package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// oldSchemaDDL 复刻迁移引擎上线前的库结构：model_configs 无 tool_rounds /
// image_support 列，thinking 为布尔语义（0=关 1=开），且没有 schema_migrations 账本。
const oldSchemaDDL = `
CREATE TABLE IF NOT EXISTS sessions (
	app_name   TEXT NOT NULL,
	user_id    TEXT NOT NULL,
	session_id TEXT NOT NULL,
	state      TEXT NOT NULL DEFAULT '{}',
	tracks     TEXT NOT NULL DEFAULT '{}',
	summaries  TEXT NOT NULL DEFAULT '{}',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (app_name, user_id, session_id)
);
CREATE TABLE IF NOT EXISTS model_configs (
	id           TEXT PRIMARY KEY,
	provider     TEXT NOT NULL DEFAULT '',
	display_name TEXT NOT NULL DEFAULT '',
	model_id     TEXT NOT NULL,
	api_format   TEXT NOT NULL DEFAULT 'openai_chat',
	base_url     TEXT NOT NULL,
	full_url     INTEGER NOT NULL DEFAULT 0,
	api_key      TEXT NOT NULL DEFAULT '',
	temperature  REAL,
	max_tokens   INTEGER,
	thinking     INTEGER NOT NULL DEFAULT 0,
	created_at   INTEGER NOT NULL,
	updated_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(app_name, user_id, updated_at);
`

func openStore(t *testing.T, name string) (*sqliteStore, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	raw, err := newSQLite(path)
	if err != nil {
		t.Fatalf("newSQLite: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return raw.(*sqliteStore), path
}

// createOldDB 造一个"迁移前"的旧库：旧结构 + 两行模型配置（thinking=1 开 / thinking=0 关）。
func createOldDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open old db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(oldSchemaDDL); err != nil {
		t.Fatalf("create old schema: %v", err)
	}
	for _, thinking := range []int{1, 0} {
		if _, err := db.Exec(
			`INSERT INTO model_configs(id, model_id, base_url, thinking, created_at, updated_at) VALUES(?,?,?,?,?,?)`,
			"mc-"+strconv.Itoa(thinking), "model-x", "http://example.invalid", thinking, 1, 1); err != nil {
			t.Fatalf("seed old row: %v", err)
		}
	}
}

func columnExists(t *testing.T, st *sqliteStore, table, col string) bool {
	t.Helper()
	var n int
	if err := st.db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('`+table+`') WHERE name=?`, col).Scan(&n); err != nil {
		t.Fatalf("pragma_table_info(%s): %v", table, err)
	}
	return n > 0
}

// bakFiles 列出目录内全部 .bak 文件名，按修改时间倒序（新→旧）。
// 测试侧独立实现排序，避免与被测实现的排序逻辑互相掩盖问题。
func bakFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	type bak struct {
		name    string
		modTime time.Time
	}
	var baks []bak
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".bak") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		baks = append(baks, bak{name: e.Name(), modTime: info.ModTime()})
	}
	sort.Slice(baks, func(i, j int) bool {
		if !baks[i].modTime.Equal(baks[j].modTime) {
			return baks[i].modTime.After(baks[j].modTime)
		}
		return baks[i].name > baks[j].name
	})
	out := make([]string, 0, len(baks))
	for _, b := range baks {
		out = append(out, b.name)
	}
	return out
}

// 全新库：DDL 直接建成最新结构，迁移为空操作但写账本，且不产出备份。
func TestMigrateFreshDB(t *testing.T) {
	st, path := openStore(t, "fresh.db")

	var count int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if count != len(migrations) {
		t.Fatalf("schema_migrations rows = %d, want %d", count, len(migrations))
	}
	var name string
	if err := st.db.QueryRow(`SELECT name FROM schema_migrations WHERE version=1`).Scan(&name); err != nil {
		t.Fatalf("read migration name: %v", err)
	}
	if name != "model_configs_tri_state" {
		t.Fatalf("migration name = %q", name)
	}
	if !st.tableExists(context.Background(), "knowledge_items") ||
		!st.tableExists(context.Background(), "knowledge_sources") {
		t.Fatal("fresh db missing knowledge tables")
	}
	var userVersion int
	if err := st.db.QueryRow(`PRAGMA user_version`).Scan(&userVersion); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if userVersion != schemaVersion {
		t.Fatalf("user_version = %d, want %d", userVersion, schemaVersion)
	}
	if !columnExists(t, st, "model_configs", "image_support") {
		t.Fatal("fresh db missing model_configs.image_support")
	}
	if baks := bakFiles(t, filepath.Dir(path)); len(baks) != 0 {
		t.Fatalf("fresh db produced backups: %v", baks)
	}
}

// 旧库：补列 + thinking 布尔→三态 + 补建知识两表，写账本，并先产出 v0→vN 备份。
func TestMigrateOldDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")
	createOldDB(t, path)

	raw, err := newSQLite(path)
	if err != nil {
		t.Fatalf("newSQLite on old db: %v", err)
	}
	st := raw.(*sqliteStore)
	t.Cleanup(func() { _ = raw.Close() })

	if !columnExists(t, st, "model_configs", "image_support") {
		t.Fatal("image_support column not added")
	}
	if !columnExists(t, st, "model_configs", "tool_rounds") {
		t.Fatal("tool_rounds column not added")
	}
	if !st.tableExists(context.Background(), "knowledge_items") ||
		!st.tableExists(context.Background(), "knowledge_sources") {
		t.Fatal("knowledge tables not created on upgrade")
	}

	thinking := map[string]int{}
	rows, err := st.db.Query(`SELECT id, thinking FROM model_configs`)
	if err != nil {
		t.Fatalf("query model_configs: %v", err)
	}
	for rows.Next() {
		var id string
		var v int
		if err := rows.Scan(&id, &v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		thinking[id] = v
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	_ = rows.Close()
	if thinking["mc-1"] != 1 || thinking["mc-0"] != 2 {
		t.Fatalf("thinking migration wrong: %v (want mc-1=1 开, mc-0=2 跟随默认)", thinking)
	}

	var applied int
	if err := st.db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if applied != schemaVersion {
		t.Fatalf("applied version = %d, want %d", applied, schemaVersion)
	}

	baks := bakFiles(t, dir)
	if len(baks) != 1 || !strings.Contains(baks[0], fmt.Sprintf(".v0-to-v%d.", schemaVersion)) {
		t.Fatalf("expected one v0-to-v%d backup, got %v", schemaVersion, baks)
	}
}

// 库版本高于代码版本：拒绝启动，防止旧代码写坏新库。
func TestMigrateRejectsNewerSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "future.db")

	first, err := newSQLite(path)
	if err != nil {
		t.Fatalf("newSQLite: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations(version, name, applied_at) VALUES(99, 'from_the_future', 0)`); err != nil {
		t.Fatalf("seed future version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	if _, err = newSQLite(path); err == nil {
		t.Fatal("expected error when db schema is newer than build")
	} else if !strings.Contains(err.Error(), "newer than this build") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// 幂等：重复 Migrate 不重复应用、不报错。
func TestMigrateIdempotent(t *testing.T) {
	st, _ := openStore(t, "idem.db")

	if err := st.Migrate(context.Background(), false); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	var count int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != len(migrations) {
		t.Fatalf("rows after re-run = %d, want %d", count, len(migrations))
	}
}

func TestPendingMigrations(t *testing.T) {
	if got := pendingMigrations(0); len(got) != len(migrations) {
		t.Fatalf("pending(0) = %d, want %d", len(got), len(migrations))
	}
	if got := pendingMigrations(schemaVersion); len(got) != 0 {
		t.Fatalf("pending(latest) = %d, want 0", len(got))
	}
	for i := 1; i < len(migrations); i++ {
		if migrations[i].Version <= migrations[i-1].Version {
			t.Fatalf("migrations not strictly increasing at %d", i)
		}
	}
}

func TestPruneBackupsKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	names := []string{
		"absorb.v0-to-v1.20260101-100000.bak",
		"absorb.v0-to-v1.20260102-100000.bak",
		"absorb.v0-to-v1.20260103-100000.bak",
		"absorb.v0-to-v1.20260104-100000.bak",
		"absorb.v0-to-v1.20260105-100000.bak",
		"absorb.db",     // 非备份文件不得被清理
		"absorb.db-wal", // WAL 伴随文件同样要保留
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", n, err)
		}
	}

	(&sqliteStore{}).pruneBackups(dir, "absorb")

	kept := bakFiles(t, dir)
	if len(kept) != backupKeep {
		t.Fatalf("kept %d backups, want %d (%v)", len(kept), backupKeep, kept)
	}
	// 倒序（新→旧）：最新的必须被保留
	if kept[0] != "absorb.v0-to-v1.20260105-100000.bak" {
		t.Fatalf("newest backup not kept: %v", kept)
	}
	for _, n := range []string{"absorb.db", "absorb.db-wal"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Fatalf("non-backup file %s was removed: %v", n, err)
		}
	}
}

// 知识两表：唯一索引承载两条数据层约束——幂等保存（dedup_key）与
// 同条目内防重复引用（quote 指纹），且多来源 1:N 正常可写。
func TestKnowledgeTables(t *testing.T) {
	st, _ := openStore(t, "knowledge.db")
	ctx := context.Background()

	insertItem := func(id string) error {
		_, err := st.db.ExecContext(ctx,
			`INSERT INTO knowledge_items(id, app_name, user_id, title, origin_session_id, origin_event_id, dedup_key, created_at, updated_at)
			 VALUES(?,?,?,?,?,?,?,?,?)`, id, "app", "user", "标题", "s1", "e1", "dk1", 1, 1)
		return err
	}
	if err := insertItem("ki-1"); err != nil {
		t.Fatalf("insert item: %v", err)
	}
	// 同 (app,user,dedup_key)：幂等保存的唯一性保证，必须拒绝
	if err := insertItem("ki-2"); err == nil {
		t.Fatal("duplicate dedup_key accepted, want unique violation")
	}

	insertSource := func(id, quote string) error {
		_, err := st.db.ExecContext(ctx,
			`INSERT INTO knowledge_sources(id, item_id, app_name, user_id, quote_text, created_at) VALUES(?,?,?,?,?,?)`,
			id, "ki-1", "app", "user", quote, 1)
		return err
	}
	if err := insertSource("ks-1", "原文一"); err != nil {
		t.Fatalf("insert source: %v", err)
	}
	// 同条目同原文：防重复引用，必须拒绝
	if err := insertSource("ks-2", "原文一"); err == nil {
		t.Fatal("duplicate source quote accepted, want unique violation")
	}
	// 同条目不同原文：多来源 1:N，必须可写
	if err := insertSource("ks-3", "原文二"); err != nil {
		t.Fatalf("insert second source: %v", err)
	}
}

func TestEscapeSQLLiteral(t *testing.T) {
	if got := escapeSQLLiteral(`C:\it's\a.db`); got != `C:\it''s\a.db` {
		t.Fatalf("escapeSQLLiteral = %q", got)
	}
}

// BackupNow / SchemaInfo：升级前保全与 status 子命令的数据源。
func TestBackupNowAndSchemaInfo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backup.db")
	createOldDB(t, path) // 旧库：打开时迁移并自动生成 v0-to-v1 备份

	raw, err := newSQLite(path)
	if err != nil {
		t.Fatalf("newSQLite: %v", err)
	}
	st := raw.(*sqliteStore)
	t.Cleanup(func() { _ = raw.Close() })
	ctx := context.Background()

	info, err := st.SchemaInfo(ctx)
	if err != nil {
		t.Fatalf("SchemaInfo: %v", err)
	}
	if info.Version != schemaVersion {
		t.Fatalf("schema version = %d, want %d", info.Version, schemaVersion)
	}
	if len(info.Backups) != 1 || !strings.Contains(info.Backups[0], fmt.Sprintf(".v0-to-v%d.", schemaVersion)) {
		t.Fatalf("backups after migration = %v", info.Backups)
	}

	dst, err := st.BackupNow(ctx)
	if err != nil {
		t.Fatalf("BackupNow: %v", err)
	}
	if dst == "" || !strings.Contains(filepath.Base(dst), ".manual.") {
		t.Fatalf("manual backup path = %q", dst)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("manual backup file missing: %v", err)
	}
	info2, err := st.SchemaInfo(ctx)
	if err != nil {
		t.Fatalf("SchemaInfo: %v", err)
	}
	if len(info2.Backups) != 2 {
		t.Fatalf("backups = %v, want 2", info2.Backups)
	}
	if !strings.Contains(info2.Backups[0], ".manual.") {
		t.Fatalf("newest backup should be the manual one: %v", info2.Backups)
	}
}

// 手动备份同样受保留份数约束。
func TestBackupPruneManual(t *testing.T) {
	st, path := openStore(t, "prune.db")
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := st.BackupNow(ctx); err != nil {
			t.Fatalf("BackupNow #%d: %v", i, err)
		}
	}
	if baks := bakFiles(t, filepath.Dir(path)); len(baks) != backupKeep {
		t.Fatalf("kept %d backups, want %d (%v)", len(baks), backupKeep, baks)
	}
}
