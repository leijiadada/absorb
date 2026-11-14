package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/session"
)

// Storage 是持久化层的统一契约。不同后端（SQLite / Postgres / 内存…）实现该接口，
// 上层 SessionService 仅依赖该接口，从而与具体存储后端解耦，可按配置快速切换。
type Storage interface {
	// 会话（对齐 session.Service 契约）
	CreateSession(ctx context.Context, key session.Key, state session.StateMap, opts ...session.Option) (*session.Session, error)
	GetSession(ctx context.Context, key session.Key, opts ...session.Option) (*session.Session, error)
	ListSessions(ctx context.Context, userKey session.UserKey, opts ...session.Option) ([]*session.Session, error)
	DeleteSession(ctx context.Context, key session.Key, opts ...session.Option) error
	UpdateAppState(ctx context.Context, appName string, state session.StateMap) error
	DeleteAppState(ctx context.Context, appName, key string) error
	ListAppStates(ctx context.Context, appName string) (session.StateMap, error)
	UpdateUserState(ctx context.Context, userKey session.UserKey, state session.StateMap) error
	ListUserStates(ctx context.Context, userKey session.UserKey) (session.StateMap, error)
	DeleteUserState(ctx context.Context, userKey session.UserKey, key string) error
	UpdateSessionState(ctx context.Context, key session.Key, state session.StateMap) error
	AppendEvent(ctx context.Context, sess *session.Session, evt *event.Event, opts ...session.Option) error

	// 参考材料重申（scope 感知：filterKey 非空仅查该 scope 含无标事件，空查全会话；
	// 宁缺勿错——该 scope 从未划词返回空，不串其他文章的参考材料）
	ReferenceMaterial(ctx context.Context, app, user, sessionID, filterKey string) (string, error)

	// 页面文本缓存（scope 感知：同会话多页面各存一行；scopeID 规范化在实现层做）
	SavePageText(ctx context.Context, app, user, sessionID, scopeID, pageTitle, pageText string) error
	LoadPageText(ctx context.Context, app, user, sessionID, scopeID string) (string, error)

	// 会话摘要持久化（跨文章长会话压缩）：summaries 整体替换写入；
	// stateMerge 为需合并进现有 state 列的增量键（摘要 boundary 状态键），
	// 单事务内读-改-写，避免覆盖并发写入的其他 state 键。
	SaveSummaries(ctx context.Context, key session.Key, summaries map[string]*session.Summary, stateMerge session.StateMap) error

	// RecentPageURL 查询某会话历史中最近一次 <page_url> 块的 URL 值。
	// 用于追问（无新划词/无页面）时继承上一轮的文章 scope。返回 ("", nil) 表示未找到。
	RecentPageURL(ctx context.Context, app, user, sessionID string) (string, error)

	// scope 标识 ↔ 网址映射（filterKey 用定长哈希，杜绝 "/" 层级误判）：
	// UpsertScopeURL 每轮提问时 upsert（last_seen 刷新）；ScopeURL/ListScopeURLs
	// 供日志富化与调试把标识还原成人类可读网址。按会话隔离，不跨会话泄露 URL。
	UpsertScopeURL(ctx context.Context, app, user, sessionID, scopeID, rawURL string) error
	ScopeURL(ctx context.Context, app, user, sessionID, scopeID string) (string, error)
	ListScopeURLs(ctx context.Context, app, user, sessionID string) (map[string]string, error)

	// LoadSummaries 读取某会话的持久化摘要（仅 summaries 列，不加载事件）。
	// 会话不存在返回 (nil, nil)；坏 JSON 降级为 nil（与 GetSession 同语义）。
	LoadSummaries(ctx context.Context, key session.Key) (map[string]*session.Summary, error)

	// 会话召回（recall_history）：复用 session_events 表的只读查询，无 schema 变更。
	// scope 语义与摘要 scopedEvents 一致：FilterKey == filterKey || FilterKey == ""；
	// filterKey 为空查全会话。覆盖判定（e.Timestamp ≤ CutoffAt）由调用方用
	// sess.Summaries 的 boundary 计算，本层不做。

	// ListEventIndex 返回某 scope 全部事件的目录条目（seq 升序）。
	ListEventIndex(ctx context.Context, key session.Key, filterKey string) ([]EventIndexEntry, error)

	// FetchEventSlice 返回 seq ∈ [fromSeq, toSeq] 闭区间内的 scope 事件（seq 升序）。
	// fromSeq > toSeq 或区间越界返回空切片。
	FetchEventSlice(ctx context.Context, key session.Key, filterKey string, fromSeq, toSeq int64) ([]RecallEvent, error)

	// SearchEvents 按 keywords 对 scope 事件全文包含匹配（大小写不敏感），
	// 命中词数打分降序、同分 seq 降序，截断至 limit（limit ≤ 0 不截断）。
	SearchEvents(ctx context.Context, key session.Key, filterKey string, keywords []string, limit int) ([]RecallEvent, error)

	// RecentGlobalRounds 按全局 seq 取某会话最近 maxRounds 个完整问答轮次（工作集 L1 数据源），
	// 返回按时间升序。轮次 = 用户消息事件 + 最终 assistant 文本事件；轮内工具事件不原样返回，
	// 仅以 ToolCount 计数供折叠标记使用。不做 scope 过滤（跨 scope 连续性正是 L1 的目的，
	// 去重由调用方按分支判定）。maxRounds <= 0 返回空；坏 JSON 行跳过；
	// 结尾无最终回答的悬挂 user 轮（取消轮）不计入。
	RecentGlobalRounds(ctx context.Context, key session.Key, maxRounds int) ([]RecentRound, error)

	// 模型配置
	ListModelConfigs(ctx context.Context) ([]ModelConfig, error)
	GetModelConfig(ctx context.Context, id string) (*ModelConfig, error)
	CreateModelConfig(ctx context.Context, mc *ModelConfig) error
	UpdateModelConfig(ctx context.Context, mc *ModelConfig) error
	DeleteModelConfig(ctx context.Context, id string) error
	GetDefaultModelConfig(ctx context.Context, app string) (*ModelConfig, error)
	GetDefaultModelID(ctx context.Context, app string) (string, error)
	SetDefaultModelID(ctx context.Context, app, id string) error
	ClearDefaultModelID(ctx context.Context, app string) error

	// 文档聚合（M2.5）：按 URL 锚定原始资料，承载来源回链与阅读摘要。
	// UpsertDocument 同 (app,user,url) 命中即回读既有行（title 变化时刷新）；
	// UpdateDocumentSummary 覆盖写 summary_* 三列并刷新 updated_at。
	UpsertDocument(ctx context.Context, app, user, url, title string) (*Document, error)
	UpdateDocumentSummary(ctx context.Context, app, user, docID, summaryText, sessionID, eventID string) error

	// 文档提取正文缓存（PDF 解读 PR2）：SaveDocumentContent 按 (app,user,url)
	// 幂等覆盖写（重新读取即刷新）；LoadDocumentContent 查无返回 (nil, nil)。
	SaveDocumentContent(ctx context.Context, app, user string, dc *DocumentContent) error
	LoadDocumentContent(ctx context.Context, app, user, url string) (*DocumentContent, error)

	// 知识持久化（M1）：item 可编辑（title/ai_summary/user_understanding），source 为不可变证据。
	// CreateKnowledge 幂等：同 (app,user,dedup_key) 已存在时不新增、不触碰既有 sources，
	// 返回既有条目与 created=false；新建时返回传入 item 与 created=true（时间戳由实现层填充）。
	// GetKnowledge 不存在返回 (nil, nil, nil)；ListKnowledge status 为空串查全部。
	// 引用完整性（创建同事务写 sources、删除同事务清 sources）由实现层保证。
	CreateKnowledge(ctx context.Context, app, user string, item *KnowledgeItem, sources []*KnowledgeSource) (*KnowledgeItem, bool, error)
	GetKnowledge(ctx context.Context, app, user, id string) (*KnowledgeItem, []*KnowledgeSource, error)
	ListKnowledge(ctx context.Context, app, user, status string) ([]*KnowledgeItem, error)
	UpdateKnowledgeText(ctx context.Context, app, user, id, title, aiSummary, userUnderstanding string) error
	SetKnowledgeStatus(ctx context.Context, app, user, id, status string) error
	DeleteKnowledge(ctx context.Context, app, user, id string) error

	// SearchKnowledgeWithSources 知识候选召回（M3.1）：任一关键词 LIKE 命中
	// title/ai_summary/user_understanding 或任一来源的 quote_text/source_title
	// 的 active 条目（(app,user) 范围隔离），返回条目 + 全部来源，limit 为返回
	// 条目硬上限（≤0 取实现默认）。打分与阈值过滤在知识服务层；keywords 为空
	// 返回空切片。关联分析（历史对话回溯）走 SearchEvents，两模块不合并。
	SearchKnowledgeWithSources(ctx context.Context, app, user string, keywords []string, limit int) ([]*KnowledgeHit, error)

	// 扩展端会话镜像（恢复同步）：ext_session_mirror 按 (app,user,session_id) 幂等
	// upsert 扩展原生会话对象 JSON（全量覆盖写）。时间戳由调用方传入（扩展端为
	// 归属事实源）；Upsert 冲突时保留既有 created_at、刷新其余列。
	// ListExtMirrors 按 updated_at 倒序；GetExtMirror 不存在返回 (nil, nil)。
	UpsertExtMirror(ctx context.Context, app, user, sessionID, title string, msgCount int, data string, createdAt, updatedAt time.Time) error
	ListExtMirrors(ctx context.Context, app, user string) ([]*ExtMirrorIndex, error)
	GetExtMirror(ctx context.Context, app, user, sessionID string) (*ExtMirrorRecord, error)

	// token 用量流水（telemetry 第一批）：InsertTokenUsage 只增不改，一次模型调用一行。
	// 统计为旁路链路，失败由调用方记日志降级，绝不阻断问答主链路。
	InsertTokenUsage(ctx context.Context, rec TokenUsageRecord) error

	// TokenUsageStats 聚合 token 用量统计（telemetry 第二批，桌面端展示层数据源）：
	// 今日（本地当日 0 点起）/全期合计/按 kind 全期分布/今日缓存命中率。
	// 纯只读旁路查询，失败由调用方记日志降级，不影响页面。
	TokenUsageStats(ctx context.Context) (TokenUsageStatsResult, error)

	// 生命周期
	InitSchema(ctx context.Context) error
	Close() error

	// BackupNow 立即做一次一致性备份，返回备份文件路径（空串=后端不支持/无需备份）。
	// 升级模块的升级前保全（PreApplyHook）调用；实现方须保证备份与当前数据一致。
	BackupNow(ctx context.Context) (string, error)

	// SchemaInfo 数据层自描述：已应用 schema 版本与备份列表（新→旧）。
	// 升级模块的 status 子命令与保全决策用；后端不支持时返回零值。
	SchemaInfo(ctx context.Context) (SchemaInfo, error)
}

// SchemaInfo 数据层自描述（升级模块 status / 保全用）。
type SchemaInfo struct {
	Version int      // schema_migrations 已应用最高版本（0=无账本）
	Backups []string // 备份文件名（新→旧）
}

// TokenUsageRecord token_usage 表行：单次 LLM 调用的 token 用量流水（只增不改）。
// ts 由调用方传入（Record.TS）；kind 为 chat/draft/summary（tokentracker.Kind*）。
type TokenUsageRecord struct {
	TS               time.Time
	Kind             string
	Model            string
	PromptTokens     int
	CompletionTokens int
	ReasoningTokens  int
	TotalTokens      int
	CachedTokens     int
}

// UsageTotals 一组 token 用量合计（今日/全期/按 kind 聚合共用；json 为前端展示字段名）。
type UsageTotals struct {
	Calls            int   `json:"calls"`
	TotalTokens      int64 `json:"totalTokens"`
	PromptTokens     int64 `json:"promptTokens"`
	CompletionTokens int64 `json:"completionTokens"`
	CachedTokens     int64 `json:"cachedTokens"`
}

// TokenUsageStatsResult token 用量统计结果（TokenUsageStats 返回）。
type TokenUsageStatsResult struct {
	Today        UsageTotals            `json:"today"`        // 本地今日（ts >= 当日 0 点 Unix）
	Total        UsageTotals            `json:"total"`        // 全部累计
	ByKind       map[string]UsageTotals `json:"byKind"`       // 按 kind 全期分组（chat/draft/summary），不存在的 kind 无键
	CacheHitRate float64                `json:"cacheHitRate"` // 今日缓存命中率 cached/prompt（0-1，无 prompt 调用时为 0）
}

// ExtMirrorIndex 扩展会话镜像索引条目（历史列表展示用，不含正文）。
type ExtMirrorIndex struct {
	SessionID string    `json:"sessionId"`
	Title     string    `json:"title"`
	MsgCount  int       `json:"msgCount"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ExtMirrorRecord 扩展会话镜像完整记录；Data 为扩展原生会话对象 JSON 原文。
type ExtMirrorRecord struct {
	SessionID string    `json:"sessionId"`
	Title     string    `json:"title"`
	MsgCount  int       `json:"msgCount"`
	Data      string    `json:"-"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// EventIndexEntry 会话事件目录条目（recall_history 的 index 数据源）。
// Covered 由组装层按摘要 boundary 判定（e.Timestamp ≤ CutoffAt），存储层不填。
type EventIndexEntry struct {
	Seq     int64
	Author  string
	Time    time.Time
	Covered bool
	Preview string // 内容预览（前若干 rune，展示层可再截）
}

// RecallEvent 召回的单条事件原文切片。
type RecallEvent struct {
	Seq     int64
	Author  string
	Time    time.Time
	Content string
}

// RecentRound 一个完整问答轮次的原始事件（工作集 L1 数据源）。
// 轮次 = 用户消息事件 + 最终 assistant 文本事件；同一轮的全部事件共享同一 FilterKey
// （同一 run 内 InjectIntoEvent 统一打标），调用方可据此做分支去重判定。
type RecentRound struct {
	UserEvent  event.Event // 轮次用户消息事件
	FinalEvent event.Event // 轮次最终 assistant 文本事件
	ToolCount  int         // 轮内工具相关事件数（工具结果与工具调用），0=纯文本轮
}

// NormalizeScopeID scope 身份的 URL 规范化（幂等）：去 fragment、scheme/host 小写、
// 去全部尾斜杠（根路径经补根恢复）、丢弃空 query 的问号、去 utm_* 跟踪参数、
// query 参数按 key 排序。非 URL 形态 trim 后原样返回。
// 身份统一：事件 FilterKey、摘要 key、页面缓存 scopeID 三处必须共用本函数，
// 否则同一文章经不同渠道链接（utm/fragment/尾斜杠/query 顺序差异）访问时，
// 缓存命中而重申/摘要 miss（双身份分裂）。放在本包供所有后端与上层共用。
func NormalizeScopeID(raw string) string {
	u := strings.TrimSpace(raw)
	if u == "" {
		return ""
	}
	parsed, err := url.Parse(u)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return u
	}
	parsed.Fragment = ""
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Path = strings.TrimRight(parsed.Path, "/") // 去全部尾斜杠（根路径经下方补根恢复）
	parsed.ForceQuery = false                         // 丢弃空 query 的 "?"
	q := parsed.Query()                               // 坏 query 键值对被静默丢弃
	for k := range q {
		if strings.HasPrefix(strings.ToLower(k), "utm_") {
			delete(q, k)
		}
	}
	parsed.RawQuery = q.Encode()
	// 补根判定必须在 query 终态之后：query 可能被 utm 过滤/静默丢弃而清空，
	// 若按处理前 RawQuery 判断会产生 "host" → "host/" 的非幂等漂移。
	if parsed.Path == "" {
		parsed.Path = "/"
	}
	return parsed.String()
}

// ScopeIDLen 是 scope 标识的十六进制字符数（64 位，碰撞概率在会话级文章数下可忽略）。
// 必须定长：不等长截断会使一个标识成为另一个的前缀，"/" 层级问题会以数字形式复活。
const ScopeIDLen = 16

// ScopeID 把文章 URL 映射为定长 scope 标识（消除 "/" 层级）。
//
// 背景：URL 天然带 "/" 层级（目录页/子页、站点根/工具页），而 SDK 的分支过滤、
// 摘要前缀聚合（SummaryFilterKeyMatchesPrefix）、scope 过滤（filterEventsInScope）
// 都把 "/" 当层级分隔符。后果：在祖先页面提问会聚合子孙摘要、按全会话事件下标
// 一刀切裁掉祖先自己的历史（静默丢上下文）。
//
// 定长十六进制哈希不含 "/" 且互不为前缀，上述全部路径自动退化为精确匹配：
//   - 聚合永不误触发（key == prefix 才命中）；
//   - event.Filter 双向前缀退化为相等判断；
//   - 无标事件（FilterKey==""）经 Filter 的空值分支照旧纳入，语义不变。
//
// 输入经 NormalizeScopeID 归一（utm/fragment/尾斜杠/query 顺序差异同键）；
// 空输入返回空串（不打标）。人类可读网址另存 scope_urls 映射表
// （日志与调试用），不参与身份判定。
func ScopeID(rawURL string) string {
	norm := NormalizeScopeID(rawURL)
	if norm == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:])[:ScopeIDLen]
}

// backendFactory 根据 dsn 构造一个 Storage 实例。
type backendFactory func(dsn string) (Storage, error)

var backends = map[string]backendFactory{}

// RegisterBackend 注册一个存储后端，供 New 按名称选择。
// 各后端在其 init() 中调用本函数完成自注册。
func RegisterBackend(name string, f backendFactory) {
	backends[name] = f
}

// New 按名称选择并构造后端实例。未注册的后端名返回错误。
func New(backend, dsn string) (Storage, error) {
	f, ok := backends[backend]
	if !ok {
		return nil, fmt.Errorf("unknown storage backend %q", backend)
	}
	return f(dsn)
}
