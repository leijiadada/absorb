package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/session"

	"absorb-agent/internal/log"
	"absorb-agent/internal/store/storage"

	// 纯 Go SQLite 驱动，无需 CGO（Windows 友好）
	_ "modernc.org/sqlite"
)

// sqliteStore 是 Storage 接口的 SQLite（modernc.org/sqlite）实现。
type sqliteStore struct {
	db *sql.DB
	// dbPath 去掉 DSN 参数后的数据库文件路径（迁移备份定位用）；内存库为空。
	dbPath string
	mu     sync.Mutex // SQLite 写串行
}

// newSQLite 打开 SQLite 文件并初始化表结构，返回 Storage。
// _txlock=immediate：每个事务开锁即取写锁，避免 deferred→write 升级时的
// SQLITE_BUSY 抖动（WAL 下读者不受影响）；迁移账本与业务写入同样受益。
func newSQLite(dsn string) (storage.Storage, error) {
	db, err := sql.Open("sqlite", dsn+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	ss := &sqliteStore{db: db, dbPath: fileDBPath(dsn)}
	if err := ss.InitSchema(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	return ss, nil
}

// fileDBPath 从 DSN 中取出数据库文件路径：剥掉查询参数，内存库返回 ""。
func fileDBPath(dsn string) string {
	if i := strings.IndexByte(dsn, '?'); i >= 0 {
		dsn = dsn[:i]
	}
	if dsn == "" || dsn == ":memory:" || strings.Contains(dsn, "mode=memory") {
		return ""
	}
	return dsn
}

// hasFileDB 是否为磁盘文件库（决定迁移前能否做 VACUUM INTO 备份）。
func (ss *sqliteStore) hasFileDB() bool {
	return ss.dbPath != ""
}

// init 自注册 sqlite 后端，使 store.New("sqlite", dsn) 可用。
func init() {
	storage.RegisterBackend("sqlite", newSQLite)
}

// Close 关闭数据库。
func (ss *sqliteStore) Close() error {
	return ss.db.Close()
}

// ReferenceMaterial 查询某会话 scope 内最近一次 <reference_material> 块内容。
// 用于追问时重申参考材料：filterKey 非空时只查该 scope（含无标事件），
// 该 scope 从未划词返回空（宁缺勿错，不串其他文章的参考材料）；为空查全会话。
// 返回 ("", nil) 表示未找到。
func (ss *sqliteStore) ReferenceMaterial(ctx context.Context, app, user, sessionID, filterKey string) (string, error) {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	return ss.latestTagContent(ctx, app, user, sessionID, filterKey,
		"<reference_material>", "</reference_material>")
}

// RecentPageURL 查询某会话历史中最近一次 <page_url> 块的 URL 值（追问继承文章 scope 用）。
// 会话级查询：本函数是 scope 解析的源头（继承发生时当前尚无 scope），不做 scope 过滤。
// 返回 ("", nil) 表示未找到。
func (ss *sqliteStore) RecentPageURL(ctx context.Context, app, user, sessionID string) (string, error) {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	return ss.latestTagContent(ctx, app, user, sessionID, "",
		"<page_url>", "</page_url>")
}

// ---------- scope 标识 ↔ 网址映射 ----------

// UpsertScopeURL 写入/刷新某会话的 scope 标识 → 归一网址映射（每轮提问调用）。
func (ss *sqliteStore) UpsertScopeURL(ctx context.Context, app, user, sessionID, scopeID, rawURL string) error {
	if scopeID == "" || strings.TrimSpace(rawURL) == "" {
		return nil
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()

	_, err := ss.db.ExecContext(ctx,
		`INSERT INTO scope_urls(app_name,user_id,session_id,scope_id,url,last_seen)
		 VALUES(?,?,?,?,?,?)
		 ON CONFLICT(app_name,user_id,session_id,scope_id)
		 DO UPDATE SET url=excluded.url, last_seen=excluded.last_seen`,
		app, user, sessionID, scopeID, rawURL, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("upsert scope url: %w", err)
	}
	return nil
}

// ScopeURL 查询单个标识对应的网址；未登记返回 ("", nil)。
func (ss *sqliteStore) ScopeURL(ctx context.Context, app, user, sessionID, scopeID string) (string, error) {
	if scopeID == "" {
		return "", nil
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()

	var u string
	err := ss.db.QueryRowContext(ctx,
		`SELECT url FROM scope_urls WHERE app_name=? AND user_id=? AND session_id=? AND scope_id=?`,
		app, user, sessionID, scopeID).Scan(&u)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("query scope url: %w", err)
	}
	return u, nil
}

// ListScopeURLs 返回某会话全部 标识 → 网址 映射（调试/冒烟断言/未来 L2 卡片用）。
func (ss *sqliteStore) ListScopeURLs(ctx context.Context, app, user, sessionID string) (map[string]string, error) {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	rows, err := ss.db.QueryContext(ctx,
		`SELECT scope_id, url FROM scope_urls WHERE app_name=? AND user_id=? AND session_id=?`,
		app, user, sessionID)
	if err != nil {
		return nil, fmt.Errorf("query scope urls: %w", err)
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var id, u string
		if err := rows.Scan(&id, &u); err != nil {
			return nil, err
		}
		out[id] = u
	}
	return out, rows.Err()
}

// latestTagContent 倒序扫描会话事件，返回最近一条含 <openTag>…</closeTag> 块的文本内容。
// filterKey 非空时只考虑该 scope 事件（含无标事件，语义与 loadScopeEvents 一致）。
// 事件先 json.Unmarshal 解码再从消息文本提取——json.Marshal 会把 "<" 转义为 \u003c，
// 对原始 JSON 做字面量搜索永远无法命中。坏 JSON 行跳过（辅助链路，单行损坏不中断查询）。
func (ss *sqliteStore) latestTagContent(
	ctx context.Context, app, user, sessionID, filterKey, openTag, closeTag string,
) (string, error) {
	rows, err := ss.db.QueryContext(ctx,
		`SELECT event FROM session_events WHERE app_name=? AND user_id=? AND session_id=? ORDER BY seq DESC`,
		app, user, sessionID)
	if err != nil {
		return "", fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var evtJSON string
		if err := rows.Scan(&evtJSON); err != nil {
			return "", err
		}
		var evt event.Event
		if err := json.Unmarshal([]byte(evtJSON), &evt); err != nil {
			continue
		}
		if filterKey != "" && evt.FilterKey != filterKey && evt.FilterKey != "" {
			continue
		}
		content := recallContent(&evt)
		_, rest, ok := strings.Cut(content, openTag)
		if !ok {
			continue
		}
		raw, _, ok := strings.Cut(rest, closeTag)
		if !ok {
			continue
		}
		return strings.TrimSpace(raw), nil
	}
	return "", rows.Err()
}

// SaveSummaries 单事务持久化会话摘要与增量 state 键：
// 读现有 state 列 → merge stateMerge → 连同 summaries 整体写回（避免覆盖并发写入的其他 state 键）。
func (ss *sqliteStore) SaveSummaries(
	ctx context.Context, key session.Key,
	summaries map[string]*session.Summary, stateMerge session.StateMap,
) error {
	if err := key.CheckSessionKey(); err != nil {
		return err
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()

	tx, err := ss.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var stateJSON string
	if err := tx.QueryRowContext(ctx,
		`SELECT state FROM sessions WHERE app_name=? AND user_id=? AND session_id=?`,
		key.AppName, key.UserID, key.SessionID).Scan(&stateJSON); err != nil {
		return err
	}
	state := session.StateMap{}
	_ = json.Unmarshal([]byte(stateJSON), &state)
	for k, v := range stateMerge {
		state[k] = v
	}
	stateBytes, _ := json.Marshal(state)
	summBytes, err := json.Marshal(summaries)
	if err != nil {
		return fmt.Errorf("marshal summaries: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET summaries=?, state=?, updated_at=? WHERE app_name=? AND user_id=? AND session_id=?`,
		string(summBytes), string(stateBytes), time.Now().Unix(), key.AppName, key.UserID, key.SessionID); err != nil {
		return err
	}
	return tx.Commit()
}

// CreateSession 幂等创建：不存在则插入，存在则不覆盖。返回新建会话对象。
func (ss *sqliteStore) CreateSession(
	ctx context.Context, key session.Key, state session.StateMap, _ ...session.Option,
) (*session.Session, error) {
	if err := key.CheckUserKey(); err != nil {
		return nil, err
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()

	var count int
	if err := ss.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE app_name=? AND user_id=? AND session_id=?`,
		key.AppName, key.UserID, key.SessionID).Scan(&count); err != nil {
		return nil, err
	}
	now := time.Now()
	if count == 0 {
		stateJSON, _ := json.Marshal(state)
		if _, err := ss.db.ExecContext(ctx,
			`INSERT INTO sessions(app_name, user_id, session_id, state, tracks, summaries, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?)`,
			key.AppName, key.UserID, key.SessionID, string(stateJSON), "{}", "{}", now.Unix(), now.Unix()); err != nil {
			return nil, err
		}
	}
	return &session.Session{
		ID:        key.SessionID,
		AppName:   key.AppName,
		UserID:    key.UserID,
		State:     state,
		UpdatedAt: now,
		CreatedAt: now,
	}, nil
}

// GetSession 读取会话（含全部历史 events）。未找到返回 (nil, nil)。
// 坏 JSON 行跳过并告警（与 latestTagContent/loadScopeEvents 语义一致）：
// 单行损坏降级为缺一条历史，不再拖垮整个会话的主链路加载。
func (ss *sqliteStore) GetSession(
	ctx context.Context, key session.Key, _ ...session.Option,
) (*session.Session, error) {
	if err := key.CheckSessionKey(); err != nil {
		return nil, err
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()

	var stateJSON, tracksJSON, summJSON string
	var created, updated int64
	err := ss.db.QueryRowContext(ctx,
		`SELECT state, tracks, summaries, created_at, updated_at FROM sessions WHERE app_name=? AND user_id=? AND session_id=?`,
		key.AppName, key.UserID, key.SessionID).Scan(&stateJSON, &tracksJSON, &summJSON, &created, &updated)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}

	var state session.StateMap
	_ = json.Unmarshal([]byte(stateJSON), &state)
	if state == nil {
		state = session.StateMap{}
	}

	// 摘要卡按 filterKey 分 scope 存 JSON map；坏数据降级为无摘要（runner 返回全量原始事件）
	var summaries map[string]*session.Summary
	if summJSON != "" {
		_ = json.Unmarshal([]byte(summJSON), &summaries)
	}

	rows, err := ss.db.QueryContext(ctx,
		`SELECT event FROM session_events WHERE app_name=? AND user_id=? AND session_id=? ORDER BY seq`,
		key.AppName, key.UserID, key.SessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	events := []event.Event{}
	for rows.Next() {
		var evtJSON string
		if err := rows.Scan(&evtJSON); err != nil {
			return nil, err
		}
		var evt event.Event
		if err := json.Unmarshal([]byte(evtJSON), &evt); err != nil {
			// 主链路同样跳坏行：单行损坏降级为缺一条历史，不中断会话加载
			log.Warnf("[Absorb] session_events 坏 JSON 行跳过 (session=%s/%s/%s): %v",
				key.AppName, key.UserID, key.SessionID, err)
			continue
		}
		events = append(events, evt)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return &session.Session{
		ID:        key.SessionID,
		AppName:   key.AppName,
		UserID:    key.UserID,
		State:     state,
		Events:    events,
		Summaries: summaries,
		UpdatedAt: time.Unix(updated, 0),
		CreatedAt: time.Unix(created, 0),
	}, nil
}

// ListSessions 列出某 user 下的全部会话（按更新时间倒序）。
func (ss *sqliteStore) ListSessions(
	ctx context.Context, userKey session.UserKey, _ ...session.Option,
) ([]*session.Session, error) {
	if err := userKey.CheckUserKey(); err != nil {
		return nil, err
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()

	rows, err := ss.db.QueryContext(ctx,
		`SELECT session_id, state, created_at, updated_at FROM sessions WHERE app_name=? AND user_id=? ORDER BY updated_at DESC`,
		userKey.AppName, userKey.UserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []*session.Session
	for rows.Next() {
		var sid, stateJSON string
		var created, updated int64
		if err := rows.Scan(&sid, &stateJSON, &created, &updated); err != nil {
			return nil, err
		}
		var state session.StateMap
		_ = json.Unmarshal([]byte(stateJSON), &state)
		list = append(list, &session.Session{
			ID:        sid,
			AppName:   userKey.AppName,
			UserID:    userKey.UserID,
			State:     state,
			UpdatedAt: time.Unix(updated, 0),
			CreatedAt: time.Unix(created, 0),
		})
	}
	return list, rows.Err()
}

// DeleteSession 删除会话及其全部 events。
func (ss *sqliteStore) DeleteSession(
	ctx context.Context, key session.Key, _ ...session.Option,
) error {
	if err := key.CheckSessionKey(); err != nil {
		return err
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if _, err := ss.db.ExecContext(ctx,
		`DELETE FROM session_events WHERE app_name=? AND user_id=? AND session_id=?`,
		key.AppName, key.UserID, key.SessionID); err != nil {
		return err
	}
	_, err := ss.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE app_name=? AND user_id=? AND session_id=?`,
		key.AppName, key.UserID, key.SessionID)
	return err
}

// UpdateSessionState 更新会话 state（整覆盖）。
func (ss *sqliteStore) UpdateSessionState(
	ctx context.Context, key session.Key, state session.StateMap,
) error {
	if err := key.CheckSessionKey(); err != nil {
		return err
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	stateJSON, _ := json.Marshal(state)
	_, err := ss.db.ExecContext(ctx,
		`UPDATE sessions SET state=?, updated_at=? WHERE app_name=? AND user_id=? AND session_id=?`,
		string(stateJSON), time.Now().Unix(), key.AppName, key.UserID, key.SessionID)
	return err
}

// AppendEvent 追加一条事件，seq 自增。会话须已存在（runner 在 CreateSession 后调用）。
func (ss *sqliteStore) AppendEvent(
	ctx context.Context, sess *session.Session, evt *event.Event, opts ...session.Option,
) error {
	if sess == nil {
		return session.ErrNilSession
	}
	// 同步内存 sess（与 inmemory 一致），runner 后续可能直接读取 sess.Events
	sess.UpdateUserSession(evt, opts...)
	ss.mu.Lock()
	defer ss.mu.Unlock()

	var maxSeq sql.NullInt64
	_ = ss.db.QueryRowContext(ctx,
		`SELECT MAX(seq) FROM session_events WHERE app_name=? AND user_id=? AND session_id=?`,
		sess.AppName, sess.UserID, sess.ID).Scan(&maxSeq)
	nextSeq := maxSeq.Int64 + 1

	// 多模态：落库前对图片 content part 降级（方案 A）——dataURL 图生成 192px 缩略图
	// 替换（控体积且跨轮保留视觉参考），外链图原样保留（仅 URL 数十字节），降级失败
	// （解码错误/不支持格式如 webp）维持旧策略丢弃。策略详见 thumbnail.go。
	// 在落库副本上降级（shallowDowngradeEvent），不得原地改写 evt：UpdateUserSession
	// 已把 parts 底层数组同步进内存 sess，原地降级会让当轮模型请求也只见缩略图——
	// 原图必须在当轮存活（SDK 请求与工具轮上下文），仅跨轮历史使用落库缩略图。
	evtJSON, err := json.Marshal(shallowDowngradeEvent(evt))
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	if _, err := ss.db.ExecContext(ctx,
		`INSERT INTO session_events(app_name, user_id, session_id, seq, event) VALUES(?,?,?,?,?)`,
		sess.AppName, sess.UserID, sess.ID, nextSeq, string(evtJSON)); err != nil {
		return err
	}
	// 把 UpdateUserSession 已合并 StateDelta 的 sess.State 写回 state 列（跨重启可恢复）
	stateJSON, _ := json.Marshal(sess.State)
	_, err = ss.db.ExecContext(ctx,
		`UPDATE sessions SET state=?, updated_at=? WHERE app_name=? AND user_id=? AND session_id=?`,
		string(stateJSON), time.Now().Unix(), sess.AppName, sess.UserID, sess.ID)
	return err
}

// ---------- 工作集 L1（全局最近对话尾部读取） ----------

// recentRoundsScanLimit L1 轮次读取的行数上限（按 seq 倒序）：约束单次查询成本。
// 两轮问答含工具链通常 ≤ 60 事件，200 行余量充足；窗口内凑不满完整轮次则返回已找到的。
const recentRoundsScanLimit = 200

// RecentGlobalRounds 按全局 seq 取某会话最近 maxRounds 个完整问答轮次（L1 数据源），
// 返回按时间升序。实现见 storage.Storage 接口注释。seq 为 (app,user,session) 会话级
// 全局递增（AppendEvent），跨 scope 时序天然成立；本层不做 scope 过滤，去重由调用方
// 按分支判定（workingset.go）。坏 JSON 行跳过 + 告警（与 GetSession/loadScopeEvents 语义一致）。
//
// 锁策略：ss.mu 是整个 store 的全局写锁。锁内只做行扫描（廉价），JSON 反序列化移到
// 锁外执行——锁内解码 200 行（含单条可达 16KB 的大工具结果事件）会阻塞其他会话的
// GetSession/AppendEvent 与摘要 worker 的 SaveSummaries。
func (ss *sqliteStore) RecentGlobalRounds(ctx context.Context, key session.Key, maxRounds int) ([]storage.RecentRound, error) {
	if err := key.CheckSessionKey(); err != nil {
		return nil, err
	}
	if maxRounds <= 0 {
		return nil, nil
	}
	ss.mu.Lock()
	rows, err := ss.db.QueryContext(ctx,
		`SELECT event FROM session_events WHERE app_name=? AND user_id=? AND session_id=? ORDER BY seq DESC LIMIT ?`,
		key.AppName, key.UserID, key.SessionID, recentRoundsScanLimit)
	if err != nil {
		ss.mu.Unlock()
		return nil, fmt.Errorf("query events: %w", err)
	}
	rawJSON := make([]string, 0, recentRoundsScanLimit)
	for rows.Next() {
		var evtJSON string
		if err := rows.Scan(&evtJSON); err != nil {
			rows.Close()
			ss.mu.Unlock()
			return nil, err
		}
		rawJSON = append(rawJSON, evtJSON)
	}
	rowsErr := rows.Err()
	rows.Close()
	ss.mu.Unlock()
	if rowsErr != nil {
		return nil, rowsErr
	}

	// 锁外反序列化：坏行跳过 + 告警（不因单行损坏中断 L1）。
	var tail []event.Event // 逆序（新→旧）的尾部窗口
	for _, evtJSON := range rawJSON {
		var evt event.Event
		if err := json.Unmarshal([]byte(evtJSON), &evt); err != nil {
			log.Warnf("[Absorb] session_events 坏 JSON 行跳过 (L1 rounds, session=%s/%s/%s): %v",
				key.AppName, key.UserID, key.SessionID, err)
			continue
		}
		tail = append(tail, evt)
	}

	// 逆序配对轮次：最新 final assistant → 向旧收集工具事件 → 遇 user 收轮。
	// 同一 run 的全部事件共享同一 FilterKey（InjectIntoEvent），一轮不会跨 scope；
	// 结尾被取消的悬挂 user 轮（无 assistant）在"跳到下一个 final"阶段被自然跳过。
	rounds := make([]storage.RecentRound, 0, maxRounds)
	i := 0
	for len(rounds) < maxRounds {
		for i < len(tail) && !isFinalAssistantText(&tail[i]) {
			i++
		}
		if i >= len(tail) {
			break
		}
		final := tail[i]
		i++
		toolCount := 0
		userIdx := -1
		for j := i; j < len(tail); j++ {
			if isUserEvent(&tail[j]) {
				userIdx = j
				break
			}
			if isToolRelated(&tail[j]) {
				toolCount++
			}
		}
		if userIdx < 0 {
			break // 窗口耗尽仍无 user，轮次不完整，放弃
		}
		rounds = append(rounds, storage.RecentRound{
			UserEvent:  tail[userIdx],
			FinalEvent: final,
			ToolCount:  toolCount,
		})
		i = userIdx + 1
	}
	// 反转为升序（旧→新）
	for l, r := 0, len(rounds)-1; l < r; l, r = l+1, r-1 {
		rounds[l], rounds[r] = rounds[r], rounds[l]
	}
	// 窗口耗尽仍凑不满请求轮数：工具密集轮次（每轮数十事件）可能把目标轮次挤出
	// 200 行窗口。静默少注入会让 L1 的跨 scope 连续性悄悄失效，必须留痕。
	if len(rounds) < maxRounds && len(rawJSON) >= recentRoundsScanLimit {
		log.Warnf("[Absorb] L1 rounds scan window exhausted (session=%s/%s/%s): got %d/%d rounds within %d events",
			key.AppName, key.UserID, key.SessionID, len(rounds), maxRounds, recentRoundsScanLimit)
	}
	return rounds, nil
}

// isFinalAssistantText 最终 assistant 文本事件：role=assistant、有非空文本、不携带工具调用。
func isFinalAssistantText(e *event.Event) bool {
	if e == nil || e.Response == nil || e.IsPartial || len(e.Response.Choices) == 0 {
		return false
	}
	m := e.Response.Choices[0].Message
	return m.Role == model.RoleAssistant && m.Content != "" && len(m.ToolCalls) == 0
}

// isUserEvent 用户消息事件。
func isUserEvent(e *event.Event) bool {
	if e == nil || e.Response == nil || len(e.Response.Choices) == 0 {
		return false
	}
	return e.Response.Choices[0].Message.Role == model.RoleUser
}

// isToolRelated 工具相关事件：工具结果（role=tool）或携带工具调用的 assistant 事件。
func isToolRelated(e *event.Event) bool {
	if e == nil || e.Response == nil || len(e.Response.Choices) == 0 {
		return false
	}
	m := e.Response.Choices[0].Message
	return m.Role == model.RoleTool || len(m.ToolCalls) > 0
}

// ---------- 会话召回（recall_history 只读查询） ----------

// recallPreviewRunes 目录条目预览的截取长度（rune）。
const recallPreviewRunes = 200

// LoadSummaries 读取某会话的持久化摘要（仅 summaries 列，不加载事件）。
// 会话不存在返回 (nil, nil)；坏 JSON 降级为 nil（与 GetSession 同语义）。
func (ss *sqliteStore) LoadSummaries(ctx context.Context, key session.Key) (map[string]*session.Summary, error) {
	if err := key.CheckSessionKey(); err != nil {
		return nil, err
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()

	var summJSON string
	err := ss.db.QueryRowContext(ctx,
		`SELECT summaries FROM sessions WHERE app_name=? AND user_id=? AND session_id=?`,
		key.AppName, key.UserID, key.SessionID).Scan(&summJSON)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if summJSON == "" {
		return nil, nil
	}
	var summaries map[string]*session.Summary
	_ = json.Unmarshal([]byte(summJSON), &summaries)
	return summaries, nil
}

// loadScopeEvents 加载某会话 scope 内的全部事件（seq 升序）。调用方须持有 ss.mu。
// scope 语义与摘要 scopedEvents 一致：filterKey 非空时保留 FilterKey==filterKey 或
// 无标（FilterKey==""）事件；filterKey 为空查全会话。坏 JSON 行跳过（召回为辅助链路，
// 单行损坏不中断整个查询）。
func (ss *sqliteStore) loadScopeEvents(ctx context.Context, key session.Key, filterKey string) ([]storage.RecallEvent, error) {
	rows, err := ss.db.QueryContext(ctx,
		`SELECT seq, event FROM session_events WHERE app_name=? AND user_id=? AND session_id=? ORDER BY seq`,
		key.AppName, key.UserID, key.SessionID)
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()

	var out []storage.RecallEvent
	for rows.Next() {
		var seq int64
		var evtJSON string
		if err := rows.Scan(&seq, &evtJSON); err != nil {
			return nil, err
		}
		var evt event.Event
		if err := json.Unmarshal([]byte(evtJSON), &evt); err != nil {
			continue
		}
		if filterKey != "" && evt.FilterKey != filterKey && evt.FilterKey != "" {
			continue
		}
		out = append(out, storage.RecallEvent{
			Seq:     seq,
			Author:  recallAuthor(&evt),
			Time:    evt.Timestamp,
			Content: recallContent(&evt),
		})
	}
	return out, rows.Err()
}

// recallAuthor 事件作者：优先 Author，回退首条消息的 Role。
func recallAuthor(e *event.Event) string {
	if e.Author != "" {
		return e.Author
	}
	if e.Response != nil && len(e.Response.Choices) > 0 {
		return string(e.Response.Choices[0].Message.Role)
	}
	return ""
}

// recallContent 拼接事件文本内容（各 Choice 的 Message.Content，换行连接）。
func recallContent(e *event.Event) string {
	if e.Response == nil {
		return ""
	}
	var b strings.Builder
	for _, ch := range e.Response.Choices {
		if ch.Message.Content == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(ch.Message.Content)
	}
	return b.String()
}

// truncateRunes rune 安全截断。
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// ListEventIndex 返回某 scope 全部事件的目录条目（seq 升序，预览截 recallPreviewRunes）。
func (ss *sqliteStore) ListEventIndex(ctx context.Context, key session.Key, filterKey string) ([]storage.EventIndexEntry, error) {
	if err := key.CheckSessionKey(); err != nil {
		return nil, err
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()

	events, err := ss.loadScopeEvents(ctx, key, filterKey)
	if err != nil {
		return nil, err
	}
	entries := make([]storage.EventIndexEntry, 0, len(events))
	for _, e := range events {
		entries = append(entries, storage.EventIndexEntry{
			Seq:     e.Seq,
			Author:  e.Author,
			Time:    e.Time,
			Preview: truncateRunes(e.Content, recallPreviewRunes),
		})
	}
	return entries, nil
}

// FetchEventSlice 返回 seq ∈ [fromSeq, toSeq] 闭区间内的 scope 事件（seq 升序）。
// fromSeq > toSeq 或区间越界返回空切片。
func (ss *sqliteStore) FetchEventSlice(ctx context.Context, key session.Key, filterKey string, fromSeq, toSeq int64) ([]storage.RecallEvent, error) {
	if err := key.CheckSessionKey(); err != nil {
		return nil, err
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()

	events, err := ss.loadScopeEvents(ctx, key, filterKey)
	if err != nil {
		return nil, err
	}
	var out []storage.RecallEvent
	for _, e := range events {
		if e.Seq >= fromSeq && e.Seq <= toSeq {
			out = append(out, e)
		}
	}
	return out, nil
}

// SearchEvents 按 keywords 全文包含匹配（大小写不敏感），命中词数打分降序、
// 同分 seq 降序，截断至 limit（limit ≤ 0 不截断）。keywords 清洗后为空返回空切片。
func (ss *sqliteStore) SearchEvents(ctx context.Context, key session.Key, filterKey string, keywords []string, limit int) ([]storage.RecallEvent, error) {
	if err := key.CheckSessionKey(); err != nil {
		return nil, err
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()

	kws := normalizeKeywords(keywords)
	if len(kws) == 0 {
		return nil, nil
	}
	events, err := ss.loadScopeEvents(ctx, key, filterKey)
	if err != nil {
		return nil, err
	}

	type scored struct {
		evt   storage.RecallEvent
		score int
	}
	var hits []scored
	for _, e := range events {
		lc := strings.ToLower(e.Content)
		score := 0
		for _, kw := range kws {
			if strings.Contains(lc, kw) {
				score++
			}
		}
		if score > 0 {
			hits = append(hits, scored{evt: e, score: score})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].evt.Seq > hits[j].evt.Seq
	})
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]storage.RecallEvent, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.evt)
	}
	return out, nil
}

// normalizeKeywords 关键词清洗：trim、小写、去空、去重（匹配统一小写比较）。
func normalizeKeywords(keywords []string) []string {
	out := make([]string, 0, len(keywords))
	seen := make(map[string]struct{}, len(keywords))
	for _, k := range keywords {
		k = strings.ToLower(strings.TrimSpace(k))
		if k == "" {
			continue
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	return out
}
