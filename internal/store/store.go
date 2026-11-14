package store

import (
	"context"
	"strings"
	"sync"

	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/session"

	"absorb-agent/internal/kind"
	"absorb-agent/internal/store/storage"

	// 自注册 sqlite 后端，使 New("sqlite", dsn) / NewSessionService 默认可用
	// （测试与主程序无需额外导入）。
	_ "absorb-agent/internal/store/sqlite"
)

// ModelConfig 即存储层模型配置类型（storage.ModelConfig 的别名，保持调用方兼容）。
type ModelConfig = storage.ModelConfig

// NormalizeScopeID 透传 storage.NormalizeScopeID（scope 身份 URL 归一）。
// 供 chat 层统一事件 FilterKey/摘要 key 与页面缓存的 scope 身份。
func NormalizeScopeID(raw string) string { return storage.NormalizeScopeID(raw) }

// ScopeID 透传 storage.ScopeID：文章 URL → 定长 scope 标识。
// 全仓唯一入口：任何地方要把 URL 当作 scope 键（事件 FilterKey/摘要 key/页面缓存
// scopeID/recall 工具入参）都必须经过本函数，禁止直接使用裸 URL，否则会分裂出
// 第二套 scope 身份（哈希与 URL 永不相等，老数据静默失效）。
func ScopeID(rawURL string) string { return storage.ScopeID(rawURL) }

// ScopeIDLen 透传 storage.ScopeIDLen：定长标识的十六进制字符数（测试与日志校验用）。
const ScopeIDLen = storage.ScopeIDLen

// SessionService 是会话持久化的上层门面：
//   - 实现 session.Service 契约（给 runner）；
//   - 暴露应用侧扩展方法（RecentReferenceMaterial / 模型配置管理等，给 handler）；
//   - 具体持久化全部委派给 storage.Storage 接口，从而与具体存储后端解耦。
type SessionService struct {
	store storage.Storage

	// 摘要异步运行时（summary.go）：SetSummarizer 注入后启用；未注入 = 功能关闭。
	sumMu sync.Mutex
	sum   *summaryRunner
}

var _ session.Service = (*SessionService)(nil)

func New(backend, dsn string) (*SessionService, error) {
	st, err := storage.New(backend, dsn)
	if err != nil {
		return nil, err
	}
	return &SessionService{store: st}, nil
}

// NewSessionService 打开/创建 SQLite 文件并初始化表结构（SQLite 后端的便捷构造）。
// 返回具体类型 *SessionService，既满足 session.Service 接口（给 runner），
// 又暴露 RecentReferenceMaterial（给 handler 做参考材料重申）。
func NewSessionService(dbPath string) (*SessionService, error) {
	return New("sqlite", dbPath)
}

// Close 停止摘要 worker 并关闭底层存储。
func (s *SessionService) Close() error {
	s.stopSummarizer()
	return s.store.Close()
}

// BackupNow 立即一致性备份（升级前保全；升级模块 PreApplyHook 调用）。
// 返回备份文件路径；后端不支持时为空串。
func (s *SessionService) BackupNow(ctx context.Context) (string, error) {
	return s.store.BackupNow(ctx)
}

// Storage 暴露底层存储句柄，供其他域模块（如 knowledge）共用同一 SQLite 实例。
// 仅限装配时注入业务服务；会话数据的读写仍应走本门面方法。
func (s *SessionService) Storage() storage.Storage { return s.store }

// SchemaInfo 数据层自描述：已应用 schema 版本 + 备份列表（升级模块 status 用）。
func (s *SessionService) SchemaInfo(ctx context.Context) (storage.SchemaInfo, error) {
	return s.store.SchemaInfo(ctx)
}

// ---------- Session（委派给 Storage） ----------

func (s *SessionService) CreateSession(ctx context.Context, key session.Key, state session.StateMap, opts ...session.Option) (*session.Session, error) {
	return s.store.CreateSession(ctx, key, state, opts...)
}
func (s *SessionService) GetSession(ctx context.Context, key session.Key, opts ...session.Option) (*session.Session, error) {
	return s.store.GetSession(ctx, key, opts...)
}
func (s *SessionService) ListSessions(ctx context.Context, userKey session.UserKey, opts ...session.Option) ([]*session.Session, error) {
	return s.store.ListSessions(ctx, userKey, opts...)
}
func (s *SessionService) DeleteSession(ctx context.Context, key session.Key, opts ...session.Option) error {
	return s.store.DeleteSession(ctx, key, opts...)
}
func (s *SessionService) UpdateSessionState(ctx context.Context, key session.Key, state session.StateMap) error {
	return s.store.UpdateSessionState(ctx, key, state)
}
func (s *SessionService) AppendEvent(ctx context.Context, sess *session.Session, evt *event.Event, opts ...session.Option) error {
	return s.store.AppendEvent(ctx, sess, evt, opts...)
}
func (s *SessionService) UpdateAppState(ctx context.Context, appName string, state session.StateMap) error {
	return s.store.UpdateAppState(ctx, appName, state)
}
func (s *SessionService) ListAppStates(ctx context.Context, appName string) (session.StateMap, error) {
	return s.store.ListAppStates(ctx, appName)
}
func (s *SessionService) DeleteAppState(ctx context.Context, appName, key string) error {
	return s.store.DeleteAppState(ctx, appName, key)
}
func (s *SessionService) UpdateUserState(ctx context.Context, userKey session.UserKey, state session.StateMap) error {
	return s.store.UpdateUserState(ctx, userKey, state)
}
func (s *SessionService) ListUserStates(ctx context.Context, userKey session.UserKey) (session.StateMap, error) {
	return s.store.ListUserStates(ctx, userKey)
}
func (s *SessionService) DeleteUserState(ctx context.Context, userKey session.UserKey, key string) error {
	return s.store.DeleteUserState(ctx, userKey, key)
}

// ---------- 参考材料 / 页面文本 ----------

// RecentReferenceMaterial 查询某会话 scope 内最近一次 <reference_material> 块内容。
// 用于追问时重申参考材料：追问请求不带新划词，按当前 scope 取最近参考材料注入
// 当前 user message；scope 内从未划词返回空（宁缺勿错，不串其他文章）。
func (s *SessionService) RecentReferenceMaterial(ctx context.Context, app, user, sessionID, filterKey string) (string, error) {
	return s.store.ReferenceMaterial(ctx, app, user, sessionID, filterKey)
}
func (s *SessionService) SavePageText(ctx context.Context, app, user, sessionID, scopeID, pageTitle, pageText string) error {
	return s.store.SavePageText(ctx, app, user, sessionID, scopeID, pageTitle, pageText)
}
func (s *SessionService) LoadPageText(ctx context.Context, app, user, sessionID, scopeID string) (string, error) {
	return s.store.LoadPageText(ctx, app, user, sessionID, scopeID)
}

// RecentPageURL 查询某会话历史中最近一次 <page_url> 块的 URL 值。
// 用于追问（无新划词/无页面）时继承上一轮的文章 scope（filterKey）。
func (s *SessionService) RecentPageURL(ctx context.Context, app, user, sessionID string) (string, error) {
	return s.store.RecentPageURL(ctx, app, user, sessionID)
}

// HasDanglingToolCall 判断会话末事件是否为"悬挂 tool_calls"：最后一条持久化事件是
// 完整的 assistant 响应且携带 tool_calls、其后没有 tool 结果——即上一轮在工具调用
// 阶段被中断（用户停止 / maxRunDuration 超时）。恢复轮据此触发 WithResume 补执行。
// 判定口径与框架 llmflow.maybeResumePendingToolCalls 一致（!IsPartial + IsValidContent
// + IsToolCallResponse），避免两套标准对同一状态给出不同答案。
func (s *SessionService) HasDanglingToolCall(ctx context.Context, app, user, sessionID string) (bool, error) {
	if sessionID == "" {
		return false, nil
	}
	key := session.Key{AppName: app, UserID: user, SessionID: sessionID}
	sess, err := s.store.GetSession(ctx, key)
	if err != nil || sess == nil {
		return false, err
	}
	sess.EventMu.RLock()
	defer sess.EventMu.RUnlock()
	if len(sess.Events) == 0 {
		return false, nil
	}
	last := sess.Events[len(sess.Events)-1]
	return last.Response != nil && !last.IsPartial &&
		last.IsValidContent() && last.Response.IsToolCallResponse(), nil
}

// ---------- scope 标识 ↔ 网址映射 ----------

// UpsertScopeURL 写入/刷新 scope 标识 → 归一网址映射（每轮提问调用）。
func (s *SessionService) UpsertScopeURL(ctx context.Context, app, user, sessionID, scopeID, rawURL string) error {
	return s.store.UpsertScopeURL(ctx, app, user, sessionID, scopeID, rawURL)
}

// ScopeURL 查询标识对应网址（日志富化用）；未登记返回 ("", nil)。
func (s *SessionService) ScopeURL(ctx context.Context, app, user, sessionID, scopeID string) (string, error) {
	return s.store.ScopeURL(ctx, app, user, sessionID, scopeID)
}

// ListScopeURLs 返回会话全部 标识 → 网址 映射（调试/测试用）。
func (s *SessionService) ListScopeURLs(ctx context.Context, app, user, sessionID string) (map[string]string, error) {
	return s.store.ListScopeURLs(ctx, app, user, sessionID)
}

// RecentGlobalRounds 查询某会话全局最近的完整问答轮次（工作集 L1 数据源）。
func (s *SessionService) RecentGlobalRounds(ctx context.Context, key session.Key, maxRounds int) ([]storage.RecentRound, error) {
	return s.store.RecentGlobalRounds(ctx, key, maxRounds)
}

// InsertTokenUsage 追加一条 token 用量流水（telemetry 第一批，委派 Storage）。
func (s *SessionService) InsertTokenUsage(ctx context.Context, rec storage.TokenUsageRecord) error {
	return s.store.InsertTokenUsage(ctx, rec)
}

// TokenUsageStats token 用量统计（telemetry 第二批，委派 Storage）：
// 今日/全期合计/按 kind 分布/今日缓存命中率，桌面端展示层数据源。
func (s *SessionService) TokenUsageStats(ctx context.Context) (storage.TokenUsageStatsResult, error) {
	return s.store.TokenUsageStats(ctx)
}

// ---------- Model Configs（委派给 Storage） ----------

func (s *SessionService) ListModelConfigs(ctx context.Context) ([]ModelConfig, error) {
	return s.store.ListModelConfigs(ctx)
}
func (s *SessionService) GetModelConfig(ctx context.Context, id string) (*ModelConfig, error) {
	return s.store.GetModelConfig(ctx, id)
}
func (s *SessionService) CreateModelConfig(ctx context.Context, mc *ModelConfig) error {
	return s.store.CreateModelConfig(ctx, mc)
}
func (s *SessionService) UpdateModelConfig(ctx context.Context, mc *ModelConfig) error {
	return s.store.UpdateModelConfig(ctx, mc)
}
func (s *SessionService) DeleteModelConfig(ctx context.Context, id string) error {
	return s.store.DeleteModelConfig(ctx, id)
}
func (s *SessionService) GetDefaultModelConfig(ctx context.Context, app string) (*ModelConfig, error) {
	return s.store.GetDefaultModelConfig(ctx, app)
}
func (s *SessionService) GetDefaultModelID(ctx context.Context, app string) (string, error) {
	return s.store.GetDefaultModelID(ctx, app)
}
func (s *SessionService) SetDefaultModelID(ctx context.Context, app, id string) error {
	return s.store.SetDefaultModelID(ctx, app, id)
}
func (s *SessionService) ClearDefaultModelID(ctx context.Context, app string) error {
	return s.store.ClearDefaultModelID(ctx, app)
}

// ---------- Summaries（实现见 summary.go；未注入 summarizer 时全部为空行为） ----------

// ---------- 迁移（领域逻辑，委派底层存储操作） ----------

// MigrateLegacyModelConfig 首启迁移：模型表为空且 config.yaml 存在完整模型配置
// （name+api_key+base_url）时导入为第一条并设默认。返回是否执行了迁移。
func (s *SessionService) MigrateLegacyModelConfig(ctx context.Context, app, name, apiKey, baseURL string,
	temperature float64, maxTokens int, thinking bool,
) (bool, error) {
	if strings.TrimSpace(name) == "" || strings.TrimSpace(apiKey) == "" || strings.TrimSpace(baseURL) == "" {
		return false, nil
	}
	existing, err := s.ListModelConfigs(ctx)
	if err != nil {
		return false, err
	}
	if len(existing) > 0 {
		return false, nil
	}
	mc := &ModelConfig{
		ID:        NewModelID(),
		Provider:  string(kind.ProviderFromURL(baseURL)),
		ModelID:   strings.TrimSpace(name),
		APIFormat: "openai_chat",
		BaseURL:   strings.TrimSpace(baseURL),
		APIKey:    strings.TrimSpace(apiKey),
		// 旧 config.yaml 的 thinking 是两态开关：true=开、false=关，映射为三态 1/2
		Thinking:     triFromBool(thinking),
		ImageSupport: true,
	}
	if temperature > 0 {
		t := temperature
		mc.Temperature = &t
	}
	if maxTokens > 0 {
		mt := maxTokens
		mc.MaxTokens = &mt
	}
	if err := s.CreateModelConfig(ctx, mc); err != nil {
		return false, err
	}
	if err := s.SetDefaultModelID(ctx, app, mc.ID); err != nil {
		return false, err
	}
	return true, nil
}
