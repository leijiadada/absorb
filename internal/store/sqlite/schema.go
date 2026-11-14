package sqlite

import (
	"context"
	"database/sql"
)

// InitSchema 创建所有表（存在则跳过）并把 schema 推进到当前版本。
// 顺序：先判定是否全新库（DDL 前无业务表）→ 执行 DDL → 跑前向迁移。
// 全新库由 DDL 直接建成最新结构，迁移对其为空操作，但仍写账本（可审计）。
func (ss *sqliteStore) InitSchema(ctx context.Context) error {
	fresh := !ss.tableExists(ctx, "sessions")
	if _, err := ss.db.ExecContext(ctx, schemaDDL); err != nil {
		return err
	}
	return ss.Migrate(ctx, fresh)
}

const baseSchemaDDL = `
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
CREATE TABLE IF NOT EXISTS session_events (
	app_name   TEXT NOT NULL,
	user_id    TEXT NOT NULL,
	session_id TEXT NOT NULL,
	seq        INTEGER NOT NULL,
	event      TEXT NOT NULL,
	PRIMARY KEY (app_name, user_id, session_id, seq)
);
CREATE TABLE IF NOT EXISTS app_states (
	app_name TEXT NOT NULL,
	key      TEXT NOT NULL,
	value    TEXT NOT NULL,
	PRIMARY KEY (app_name, key)
);
CREATE TABLE IF NOT EXISTS user_states (
	app_name TEXT NOT NULL,
	user_id  TEXT NOT NULL,
	key      TEXT NOT NULL,
	value    TEXT NOT NULL,
	PRIMARY KEY (app_name, user_id, key)
);
CREATE TABLE IF NOT EXISTS page_texts (
	app_name   TEXT NOT NULL,
	user_id    TEXT NOT NULL,
	session_id TEXT NOT NULL,
	scope_id   TEXT NOT NULL DEFAULT '',
	page_title TEXT NOT NULL DEFAULT '',
	page_text  TEXT NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (app_name, user_id, session_id, scope_id)
);
CREATE TABLE IF NOT EXISTS scope_urls (
	app_name   TEXT NOT NULL,
	user_id    TEXT NOT NULL,
	session_id TEXT NOT NULL,
	scope_id   TEXT NOT NULL,
	url        TEXT NOT NULL,
	last_seen  INTEGER NOT NULL,
	PRIMARY KEY (app_name, user_id, session_id, scope_id)
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
	tool_rounds  INTEGER,
	image_support INTEGER NOT NULL DEFAULT 1,
	created_at   INTEGER NOT NULL,
	updated_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(app_name, user_id, updated_at);
CREATE INDEX IF NOT EXISTS idx_events_session ON session_events(app_name, user_id, session_id, seq);
`

// knowledgeTablesDDL 知识持久化两张表（M1，0002 迁移）。schemaDDL 与迁移共用同一份文本，
// 保证全新库与升级库结构一致，避免双份定义漂移。
//
//	knowledge_items   可变的知识对象：title/ai_summary/user_understanding 可编辑，
//	                  status 承载归档生命周期；origin_* 是产生该知识的会话回链指针（非依赖）
//	knowledge_sources 不可变的证据：quote_text 永不被编辑路径触碰；一条知识允许多来源（1:N）；
//	                   source_document_id 指向 documents 行（同 URL 聚合锚点，M2.5）
//
// 唯一索引即两条数据层约束：uq_knowledge_dedup = 幂等保存（同一回答事件重复提交
// 不产生重复记录）；uq_knowledge_source_quote = 同条目内不重复引用同一原文。
// 不设 FOREIGN KEY（与现有全部表一致，引用完整性由知识模块同事务保证）。
const knowledgeTablesDDL = `
CREATE TABLE IF NOT EXISTS knowledge_items (
	id                 TEXT PRIMARY KEY,
	app_name           TEXT NOT NULL,
	user_id            TEXT NOT NULL,
	title              TEXT NOT NULL,
	ai_summary         TEXT NOT NULL DEFAULT '',
	user_understanding TEXT NOT NULL DEFAULT '',
	status             TEXT NOT NULL DEFAULT 'active',
	origin_session_id  TEXT NOT NULL,
	origin_event_id    TEXT NOT NULL,
	dedup_key          TEXT NOT NULL,
	created_at         INTEGER NOT NULL,
	updated_at         INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS knowledge_sources (
	id                TEXT PRIMARY KEY,
	item_id           TEXT NOT NULL,
	app_name          TEXT NOT NULL,
	user_id           TEXT NOT NULL,
	source_type       TEXT NOT NULL DEFAULT 'quote',
	source_title      TEXT NOT NULL DEFAULT '',
	source_url        TEXT NOT NULL DEFAULT '',
	quote_text        TEXT NOT NULL,
	source_session_id TEXT NOT NULL DEFAULT '',
	source_event_id   TEXT NOT NULL DEFAULT '',
	source_document_id TEXT NOT NULL DEFAULT '',
	created_at        INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_knowledge_dedup ON knowledge_items(app_name, user_id, dedup_key);
CREATE INDEX IF NOT EXISTS idx_knowledge_user ON knowledge_items(app_name, user_id, status, updated_at);
CREATE INDEX IF NOT EXISTS idx_knowledge_sources_item ON knowledge_sources(item_id);
CREATE UNIQUE INDEX IF NOT EXISTS uq_knowledge_source_quote ON knowledge_sources(item_id, source_type, quote_text);
`

// documentsDDL 原始资料聚合表（M2.5，0003 迁移）。按 URL 聚合同一篇文章：
// 多条 knowledge_sources 与（未来的）阅读摘要共用一个 Document 锚点。
// summary_* 三列是"阅读摘要"第一版落点（设计文档 §五：第一版作为文档可选字段，
// 有版本管理需求时再拆表）；quote/来源定位归 knowledge_sources，职责不混。
// url 唯一索引 = 聚合键（第一版原样存 URL 不做规范化）；无 FOREIGN KEY 同全库约定。
const documentsDDL = `
CREATE TABLE IF NOT EXISTS documents (
	id                 TEXT PRIMARY KEY,
	app_name           TEXT NOT NULL,
	user_id            TEXT NOT NULL,
	url                TEXT NOT NULL,
	title              TEXT NOT NULL DEFAULT '',
	summary_text       TEXT NOT NULL DEFAULT '',
	summary_session_id TEXT NOT NULL DEFAULT '',
	summary_event_id   TEXT NOT NULL DEFAULT '',
	created_at         INTEGER NOT NULL,
	updated_at         INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_documents_url ON documents(app_name, user_id, url);
`

// TraceTablesDDL 链路追踪表（obs telemetry sqlite 落库形态，0004 迁移）。
// schema 建库、0004 迁移、obs exporter 兜底建表三方共用单一文本源。
//
//	request_traces 一次问答 = 一棵 span 树：root 行 parent_span_id 为空串，
//	即列表查询锚点（/api/traces 取最近 N 条 root 行）。attributes 是经过
//	obs redact 规则过滤后的 JSON（消息正文剔除、工具参数截断），业务归因
//	字段（keywords/zero_hit/candidate_count 等）原样入 JSON；app/user/session/
//	request 四列由 exporter 从属性提取成独立列，便于 SQL 直查。
const TraceTablesDDL = `
CREATE TABLE IF NOT EXISTS request_traces (
	trace_id       TEXT NOT NULL,
	span_id        TEXT NOT NULL,
	parent_span_id TEXT NOT NULL DEFAULT '',
	name           TEXT NOT NULL,
	start_us       INTEGER NOT NULL,
	end_us         INTEGER NOT NULL,
	duration_us    INTEGER NOT NULL,
	status         TEXT NOT NULL DEFAULT 'unset',
	attributes     TEXT NOT NULL DEFAULT '{}',
	app_name       TEXT NOT NULL DEFAULT '',
	user_id        TEXT NOT NULL DEFAULT '',
	session_id     TEXT NOT NULL DEFAULT '',
	request_id     TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (trace_id, span_id)
);
CREATE INDEX IF NOT EXISTS idx_traces_root ON request_traces(parent_span_id, start_us DESC);
CREATE INDEX IF NOT EXISTS idx_traces_session ON request_traces(session_id, start_us);
`

// extMirrorDDL 扩展端会话镜像表（0005 迁移）。扩展端把原生会话对象 JSON
// 全量幂等覆盖到本表，作为重装/清空 storage 后的恢复数据源（旁路镜像：
// 框架 session_events 落库链路不动，两表互不依赖）。title/msg_count 提升为
// 列，索引列表查询免解析全量 JSON；smoke/CLI 会话不写镜像，天然不进扩展历史。
const extMirrorDDL = `
CREATE TABLE IF NOT EXISTS ext_session_mirror (
	app_name   TEXT NOT NULL,
	user_id    TEXT NOT NULL,
	session_id TEXT NOT NULL,
	title      TEXT NOT NULL DEFAULT '',
	msg_count  INTEGER NOT NULL DEFAULT 0,
	data       TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (app_name, user_id, session_id)
);
CREATE INDEX IF NOT EXISTS idx_ext_mirror_user ON ext_session_mirror(app_name, user_id, updated_at);
`

// tokenUsageDDL LLM token 用量流水表（0006 迁移，telemetry 第一批）。一次模型调用
// 一行（kind=chat/draft/summary），由 tokentracker 经 sink 异步写入：只增不改、
// 无主键（隐含 rowid），聚合查询留给后续批次。ts 为 Unix 秒（与全库时间戳约定
// 一致）；cached_tokens 为 prompt 缓存命中；无 FOREIGN KEY 同全库约定。
const tokenUsageDDL = `
CREATE TABLE IF NOT EXISTS token_usage (
	ts                INTEGER NOT NULL,
	kind              TEXT NOT NULL DEFAULT '',
	model             TEXT NOT NULL DEFAULT '',
	prompt_tokens     INTEGER NOT NULL DEFAULT 0,
	completion_tokens INTEGER NOT NULL DEFAULT 0,
	reasoning_tokens  INTEGER NOT NULL DEFAULT 0,
	total_tokens      INTEGER NOT NULL DEFAULT 0,
	cached_tokens     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_token_usage_ts ON token_usage(ts);
`

// documentContentsDDL 文档提取正文缓存表（PDF 解读 PR2，0007 迁移）。documents
// 负责文档身份（URL 锚点），本表承载提取产物：content_text 保存完整正文（不进
// summary_text、不受聊天上下文限制），问答注入时才按 PageTextMaxLen 截断。
// extraction_status 为产品状态（ready/partial/too_large/needs_auth/no_text/
// encrypted/invalid_url/network_error，见 internal/document/extract）；失败状态
// 不落库（扩展端持临时状态，重试即重新准备），落库的只有可复用正文。
// UNIQUE(app,user,url) 与 documents 同聚合键；content_hash 供未来变更检测；
// extraction_version 标记提取器版本（升级提取逻辑后可按版本重提取）。
// 无 FOREIGN KEY 同全库约定。
const documentContentsDDL = `
CREATE TABLE IF NOT EXISTS document_contents (
	app_name            TEXT NOT NULL,
	user_id             TEXT NOT NULL,
	url                 TEXT NOT NULL,
	content_text        TEXT NOT NULL,
	char_count          INTEGER NOT NULL DEFAULT 0,
	content_type        TEXT NOT NULL DEFAULT '',
	byte_size           INTEGER NOT NULL DEFAULT 0,
	content_hash        TEXT NOT NULL DEFAULT '',
	extraction_status   TEXT NOT NULL DEFAULT '',
	extraction_version  INTEGER NOT NULL DEFAULT 1,
	truncated           INTEGER NOT NULL DEFAULT 0,
	fetched_at          INTEGER NOT NULL,
	updated_at          INTEGER NOT NULL,
	PRIMARY KEY (app_name, user_id, url)
);
`

// schemaDDL 全新库的建库文本：基础表 + 知识表 + 资料表 + 追踪表 + 扩展镜像表 +
// token 用量表 + 文档正文表（升级库由 0002~0007 迁移补建）。
const schemaDDL = baseSchemaDDL + knowledgeTablesDDL + documentsDDL + TraceTablesDDL + extMirrorDDL + tokenUsageDDL + documentContentsDDL

// migrateTraceTable 0004：建 request_traces 链路追踪表（obs sqlite 落库形态）。
// 只追加新表，不触碰任何既有表/行；DDL 与 schemaDDL、obs exporter 共用
// TraceTablesDDL 单一文本源；CREATE TABLE IF NOT EXISTS 幂等，可安全重跑。
func migrateTraceTable(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, TraceTablesDDL)
	return err
}

// migrateTokenUsage 0006：建 token_usage token 用量流水表（telemetry 第一批）。
// 只追加新表，不触碰任何既有表/行；DDL 与 schemaDDL 共用 tokenUsageDDL 单一
// 文本源；CREATE TABLE IF NOT EXISTS 幂等，可安全重跑。
func migrateTokenUsage(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, tokenUsageDDL)
	return err
}

// migrateExtMirror 0005：建 ext_session_mirror 扩展端会话镜像表。
// 只追加新表，不触碰任何既有表/行；DDL 与 schemaDDL 共用 extMirrorDDL
// 单一文本源；CREATE TABLE IF NOT EXISTS 幂等，可安全重跑。
func migrateExtMirror(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, extMirrorDDL)
	return err
}

// migrateDocumentContents 0007：建 document_contents 文档提取正文缓存表
// （PDF 解读 PR2）。只追加新表，不触碰任何既有表/行；DDL 与 schemaDDL 共用
// documentContentsDDL 单一文本源；CREATE TABLE IF NOT EXISTS 幂等，可安全重跑。
func migrateDocumentContents(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, documentContentsDDL)
	return err
}
