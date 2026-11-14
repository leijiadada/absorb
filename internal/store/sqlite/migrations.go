package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"absorb-agent/internal/store/storage"
)

// ---------- schema 迁移引擎 ----------
//
// 设计约束（详见 design/migration.md）：
//   - 迁移由「新代码启动时」执行，不由「旧代码驱动的升级流程」执行——旧二进制
//     不可能知道新 schema，且升级入口可被绕过（手动替换 exe / 绿色版 / 批量部署）。
//   - 版本权威是 schema_migrations 账本，用单调递增整数，不用 semver 文本
//     （避免 "0.10.0" < "0.9.0" 字典序陷阱）。
//   - 每条迁移一个事务，DDL 与账本写入同事务提交：中途崩溃自动回滚，下次启动重跑。
//   - 有 pending 迁移时先自动备份（VACUUM INTO，一致性由 SQLite 保证）。
//   - 库版本高于代码版本时拒绝启动，防止旧代码写坏新库（降级保护）。

// schemaVersion 本版本代码所支持的数据库 schema 版本。
// 每次增改表结构或数据语义：migrations 追加一条，并把此常量 +1。
const schemaVersion = 7

// migration 一条前向迁移。Up 在单个事务内执行。
type migration struct {
	Version int
	Name    string
	Up      func(ctx context.Context, tx *sql.Tx) error
}

// migrations 前向迁移清单（按 Version 升序）。
// 规则：只许追加到末尾且 Version = 上一条 + 1；禁止修改已发布条目——已应用过的库
// 不会重跑，改动会让不同用户的库停留在不一致状态。
var migrations = []migration{
	{
		Version: 1,
		Name:    "model_configs_tri_state",
		Up:      migrateModelConfigs,
	},
	{
		Version: 2,
		Name:    "knowledge_tables",
		Up:      migrateKnowledgeTables,
	},
	{
		Version: 3,
		Name:    "documents_and_source_doc_id",
		Up:      migrateDocuments,
	},
	{
		Version: 4,
		Name:    "request_traces",
		Up:      migrateTraceTable,
	},
	{
		Version: 5,
		Name:    "ext_session_mirror",
		Up:      migrateExtMirror,
	},
	{
		Version: 6,
		Name:    "token_usage",
		Up:      migrateTokenUsage,
	},
	{
		Version: 7,
		Name:    "document_contents",
		Up:      migrateDocumentContents,
	},
}

// migrateModelConfigs 0001：补 model_configs 新列，并把 thinking 的旧布尔语义
// （0=关 1=开）迁移为三态（0=跟随默认 1=开 2=关）。
// 新库由 schemaDDL 直接建成含新列，本迁移对其为空操作（幂等，可安全重跑）。
func migrateModelConfigs(ctx context.Context, tx *sql.Tx) error {
	var cols int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('model_configs') WHERE name='image_support'`).Scan(&cols); err != nil {
		return err
	}
	if cols > 0 {
		return nil // 新库：DDL 已含新列
	}
	for _, ddl := range []string{
		`ALTER TABLE model_configs ADD COLUMN tool_rounds INTEGER`,
		`ALTER TABLE model_configs ADD COLUMN image_support INTEGER NOT NULL DEFAULT 1`,
	} {
		if _, err := tx.ExecContext(ctx, ddl); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE model_configs SET thinking = CASE WHEN thinking = 1 THEN 1 ELSE 2 END`)
	return err
}

// migrateKnowledgeTables 0002：补建知识持久化两张表与索引（M1）。
// 只追加新表，不触碰任何既有表/行——"数据库升级不破坏既有会话和用户画像数据"
// 由结构保证。DDL 与 schemaDDL 共用 knowledgeTablesDDL 单一文本源；
// CREATE TABLE IF NOT EXISTS 天然幂等，新库上为空操作，可安全重跑。
func migrateKnowledgeTables(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, knowledgeTablesDDL)
	return err
}

// migrateDocuments 0003：建 documents 聚合表，并给 knowledge_sources 补
// source_document_id 列（M2.5 总结/吸收分离：Document 锚点）。DDL 与 schemaDDL
// 共用单一文本源；加列走 pragma 守卫（同 0001 范本），新库上为空操作。
// 开发环境不清洗旧 manual 来源行（重装即净）；conversation 语义由服务层强制。
func migrateDocuments(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, documentsDDL); err != nil {
		return err
	}
	var cols int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('knowledge_sources') WHERE name='source_document_id'`).Scan(&cols); err != nil {
		return err
	}
	if cols > 0 {
		return nil // 新库：DDL 已含新列
	}
	_, err := tx.ExecContext(ctx,
		`ALTER TABLE knowledge_sources ADD COLUMN source_document_id TEXT NOT NULL DEFAULT ''`)
	return err
}

const migrationsTableDDL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
	version    INTEGER PRIMARY KEY,
	name       TEXT NOT NULL,
	applied_at INTEGER NOT NULL
)`

// backupKeep 迁移前自动备份的保留份数（按备份名倒序保留，超出删除）。
const backupKeep = 3

// Migrate 把数据库推进到 schemaVersion：建账本 → 读已应用版本 → 有 pending 则先备份 →
// 顺序应用 → 同步 PRAGMA user_version。每次启动都调用；无 pending 时仅一次查询。
// fresh 表示本次调用前库里没有任何业务表（全新库）：跳过备份，避免首启产出空库备份。
func (ss *sqliteStore) Migrate(ctx context.Context, fresh bool) error {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	if err := ss.ensureMigrationsTable(ctx); err != nil {
		return err
	}
	applied, err := ss.appliedVersion(ctx)
	if err != nil {
		return err
	}
	if applied > schemaVersion {
		// 库由更新的应用版本创建/迁移过：旧代码继续写可能损坏数据，拒绝启动。
		return fmt.Errorf("database schema v%d is newer than this build v%d; please upgrade Absorb (a backup of the old database is kept next to it)",
			applied, schemaVersion)
	}
	pending := pendingMigrations(applied)
	if len(pending) == 0 {
		return nil
	}
	if !fresh {
		label := fmt.Sprintf("v%d-to-v%d", applied, schemaVersion)
		if _, err := ss.backup(ctx, label); err != nil {
			return err
		}
	}
	for _, m := range pending {
		if err := ss.applyMigration(ctx, m); err != nil {
			return fmt.Errorf("apply migration %04d %s: %w", m.Version, m.Name, err)
		}
	}
	if _, err := ss.db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return fmt.Errorf("set user_version: %w", err)
	}
	return nil
}

func (ss *sqliteStore) ensureMigrationsTable(ctx context.Context) error {
	_, err := ss.db.ExecContext(ctx, migrationsTableDDL)
	return err
}

// appliedVersion 账本中的最高已应用版本；空账本返回 0。
func (ss *sqliteStore) appliedVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	if err := ss.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, err
	}
	if !v.Valid {
		return 0, nil
	}
	return int(v.Int64), nil
}

// pendingMigrations 返回待应用迁移（version > applied，升序）。
func pendingMigrations(applied int) []migration {
	var out []migration
	for _, m := range migrations {
		if m.Version > applied {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out
}

// applyMigration 单条迁移：一个事务内执行 Up + 写账本，同事务提交（原子 + 幂等）。
func (ss *sqliteStore) applyMigration(ctx context.Context, m migration) error {
	tx, err := ss.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := m.Up(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations(version, name, applied_at) VALUES(?,?,?)`,
		m.Version, m.Name, time.Now().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// tableExists 判断表是否存在（用于判定全新库）。
func (ss *sqliteStore) tableExists(ctx context.Context, name string) bool {
	var n int
	_ = ss.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n)
	return n > 0
}

// BackupNow 立即做一次一致性备份，返回备份文件路径（空串=非文件库，无需备份）。
// 升级模块的 PreApplyHook 调用它做升级前保全；不做任何 schema 变更。
func (ss *sqliteStore) BackupNow(ctx context.Context) (string, error) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.backup(ctx, "manual")
}

// SchemaInfo 数据层自描述：已应用 schema 版本 + 备份列表（新→旧）。
// 升级模块的 status 子命令与保全决策用；本方法会确保账本表存在（幂等）。
func (ss *sqliteStore) SchemaInfo(ctx context.Context) (storage.SchemaInfo, error) {
	if err := ss.ensureMigrationsTable(ctx); err != nil {
		return storage.SchemaInfo{}, err
	}
	version, err := ss.appliedVersion(ctx)
	if err != nil {
		return storage.SchemaInfo{}, err
	}
	return storage.SchemaInfo{Version: version, Backups: ss.backupNames()}, nil
}

// backup 一致性备份：VACUUM INTO 产出紧凑单文件快照，一致性由 SQLite 自身保证
// （无需停写、无需处理 -wal/-shm），失败即报错，不会静默产出坏副本。
// label 标注备份原因（迁移为 "v<from>-to-v<to>"，手动保全为 "manual"）。
func (ss *sqliteStore) backup(ctx context.Context, label string) (string, error) {
	if !ss.hasFileDB() {
		return "", nil // 内存库等无文件场景：跳过备份
	}
	dir := filepath.Dir(ss.dbPath)
	base := strings.TrimSuffix(filepath.Base(ss.dbPath), filepath.Ext(ss.dbPath))
	stamp := time.Now().Format("20060102-150405")
	dest := filepath.Join(dir, fmt.Sprintf("%s.%s.%s.bak", base, label, stamp))
	// 同名（同一秒内两次备份）时追加序号，避免 VACUUM INTO 因目标已存在而失败。
	for i := 2; ; i++ {
		if _, err := os.Stat(dest); os.IsNotExist(err) {
			break
		}
		dest = filepath.Join(dir, fmt.Sprintf("%s.%s.%s-%d.bak", base, label, stamp, i))
	}
	if _, err := ss.db.ExecContext(ctx, fmt.Sprintf("VACUUM INTO '%s'", escapeSQLLiteral(dest))); err != nil {
		return "", fmt.Errorf("backup: %w", err)
	}
	ss.pruneBackups(dir, base)
	return dest, nil
}

// backupNames 备份文件名（新→旧，按修改时间）。
func (ss *sqliteStore) backupNames() []string {
	if !ss.hasFileDB() {
		return nil
	}
	return listBackups(filepath.Dir(ss.dbPath),
		strings.TrimSuffix(filepath.Base(ss.dbPath), filepath.Ext(ss.dbPath)))
}

// listBackups 列出目录内某库的备份文件名，按修改时间倒序（新→旧）；
// 同一时刻创建的多个备份按文件名倒序稳定区分。不能依赖名字里的时间戳排序——
// 同一秒内的多次备份时间戳相同，字典序会给出错误的新旧关系。
func listBackups(dir, base string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type bak struct {
		name    string
		modTime time.Time
	}
	var baks []bak
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, base+".") || !strings.HasSuffix(name, ".bak") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		baks = append(baks, bak{name: name, modTime: info.ModTime()})
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

// pruneBackups 只保留最近 backupKeep 份备份，超出按新旧倒序删除。
func (ss *sqliteStore) pruneBackups(dir, base string) {
	names := listBackups(dir, base)
	if len(names) <= backupKeep {
		return
	}
	for _, name := range names[backupKeep:] {
		_ = os.Remove(filepath.Join(dir, name))
	}
}

// escapeSQLLiteral 转义 SQL 字符串字面量中的单引号（VACUUM INTO 的目标路径）。
func escapeSQLLiteral(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}
