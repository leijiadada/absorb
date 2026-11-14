package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"absorb-agent/internal/store/storage"
)

// 知识持久化（M1）：knowledge_items / knowledge_sources 两表。
// 引用完整性约定（无外键，与全库一致）：创建在单事务内写 item + sources，
// 删除在单事务内清 sources + item；编辑路径（UpdateKnowledgeText）语句层面
// 不触碰 sources——“改提炼不改原始引用”由此保证。

// queryRower 统一 *sql.DB / *sql.Tx 的 QueryRowContext，供读行辅助函数复用。
type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const knowledgeItemCols = `id, app_name, user_id, title, ai_summary, user_understanding, status,
	origin_session_id, origin_event_id, dedup_key, created_at, updated_at`

const knowledgeSourceCols = `id, item_id, app_name, user_id, source_type, source_title, source_url,
	quote_text, source_session_id, source_event_id, source_document_id, created_at`

// scanKnowledgeItem 读出一行 knowledge_items。
func scanKnowledgeItem(sc scanner) (*storage.KnowledgeItem, error) {
	var it storage.KnowledgeItem
	var created, updated int64
	if err := sc.Scan(&it.ID, &it.AppName, &it.UserID, &it.Title, &it.AISummary, &it.UserUnderstanding,
		&it.Status, &it.OriginSessionID, &it.OriginEventID, &it.DedupKey, &created, &updated); err != nil {
		return nil, err
	}
	it.CreatedAt = time.Unix(created, 0)
	it.UpdatedAt = time.Unix(updated, 0)
	return &it, nil
}

// scanKnowledgeSource 读出一行 knowledge_sources。
func scanKnowledgeSource(sc scanner) (*storage.KnowledgeSource, error) {
	var src storage.KnowledgeSource
	var created int64
	if err := sc.Scan(&src.ID, &src.ItemID, &src.AppName, &src.UserID, &src.SourceType, &src.SourceTitle,
		&src.SourceURL, &src.QuoteText, &src.SourceSessionID, &src.SourceEventID, &src.SourceDocumentID, &created); err != nil {
		return nil, err
	}
	src.CreatedAt = time.Unix(created, 0)
	return &src, nil
}

// getKnowledgeItemRow 按任意条件读单条知识条目；无行返回 sql.ErrNoRows 由调用方定性。
func getKnowledgeItemRow(ctx context.Context, q queryRower, query string, args ...any) (*storage.KnowledgeItem, error) {
	return scanKnowledgeItem(q.QueryRowContext(ctx, query, args...))
}

// CreateKnowledge 幂等创建：单事务写 item + N 条 sources；同 (app,user,dedup_key)
// 已存在时（INSERT OR IGNORE 未命中）回读既有条目返回 created=false，不新增、
// 不触碰既有 sources。时间戳由本层填充。
func (ss *sqliteStore) CreateKnowledge(ctx context.Context, app, user string, item *storage.KnowledgeItem, sources []*storage.KnowledgeSource) (*storage.KnowledgeItem, bool, error) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	tx, err := ss.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	res, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO knowledge_items(`+knowledgeItemCols+`)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		item.ID, app, user, item.Title, item.AISummary, item.UserUnderstanding, item.Status,
		item.OriginSessionID, item.OriginEventID, item.DedupKey, now, now)
	if err != nil {
		return nil, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, false, err
	}
	if n == 0 {
		// 幂等命中：dedup 唯一索引拦下重复提交，回读既有条目（原子，无双击竞态窗口）
		existing, err := getKnowledgeItemRow(ctx, tx,
			`SELECT `+knowledgeItemCols+` FROM knowledge_items WHERE app_name=? AND user_id=? AND dedup_key=?`,
			app, user, item.DedupKey)
		if err != nil {
			return nil, false, err
		}
		return existing, false, nil
	}
	for _, src := range sources {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO knowledge_sources(`+knowledgeSourceCols+`)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			src.ID, item.ID, app, user, src.SourceType, src.SourceTitle, src.SourceURL,
			src.QuoteText, src.SourceSessionID, src.SourceEventID, src.SourceDocumentID, now); err != nil {
			return nil, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	// 回填本层填充的时间戳：调用方拿到的快照须与库内一致（否则前端看到零值时间）
	item.CreatedAt = time.Unix(now, 0)
	item.UpdatedAt = item.CreatedAt
	return item, true, nil
}

// GetKnowledge 按 ID 读取条目与全部来源（来源按写入顺序）；不存在返回 (nil, nil, nil)。
func (ss *sqliteStore) GetKnowledge(ctx context.Context, app, user, id string) (*storage.KnowledgeItem, []*storage.KnowledgeSource, error) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	item, err := getKnowledgeItemRow(ctx, ss.db,
		`SELECT `+knowledgeItemCols+` FROM knowledge_items WHERE app_name=? AND user_id=? AND id=?`,
		app, user, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	rows, err := ss.db.QueryContext(ctx,
		`SELECT `+knowledgeSourceCols+` FROM knowledge_sources WHERE app_name=? AND user_id=? AND item_id=? ORDER BY created_at, rowid`,
		app, user, id)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var sources []*storage.KnowledgeSource
	for rows.Next() {
		src, err := scanKnowledgeSource(rows)
		if err != nil {
			return nil, nil, err
		}
		sources = append(sources, src)
	}
	return item, sources, rows.Err()
}

// ListKnowledge 按 status 过滤列出条目（status 空串=全部），按更新时间新→旧。
// 不带 sources（列表无此需求；详情走 GetKnowledge，避免 N+1）。
func (ss *sqliteStore) ListKnowledge(ctx context.Context, app, user, status string) ([]*storage.KnowledgeItem, error) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	query := `SELECT ` + knowledgeItemCols + ` FROM knowledge_items WHERE app_name=? AND user_id=?`
	args := []any{app, user}
	if status != "" {
		query += ` AND status=?`
		args = append(args, status)
	}
	query += ` ORDER BY updated_at DESC, rowid DESC`
	rows, err := ss.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*storage.KnowledgeItem
	for rows.Next() {
		it, err := scanKnowledgeItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// UpdateKnowledgeText 编辑可变文本三列（title/ai_summary/user_understanding），
// 刷新 updated_at。语句层面不触碰 knowledge_sources。
func (ss *sqliteStore) UpdateKnowledgeText(ctx context.Context, app, user, id, title, aiSummary, userUnderstanding string) error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	_, err := ss.db.ExecContext(ctx,
		`UPDATE knowledge_items SET title=?, ai_summary=?, user_understanding=?, updated_at=?
		 WHERE app_name=? AND user_id=? AND id=?`,
		title, aiSummary, userUnderstanding, time.Now().Unix(), app, user, id)
	return err
}

// SetKnowledgeStatus 归档/重开（软删除语义），刷新 updated_at。
func (ss *sqliteStore) SetKnowledgeStatus(ctx context.Context, app, user, id, status string) error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	_, err := ss.db.ExecContext(ctx,
		`UPDATE knowledge_items SET status=?, updated_at=? WHERE app_name=? AND user_id=? AND id=?`,
		status, time.Now().Unix(), app, user, id)
	return err
}

// DeleteKnowledge 硬删除：单事务先清 sources 再删 item（引用完整性由本层保证）；
// 目标不存在时为幂等空操作。
func (ss *sqliteStore) DeleteKnowledge(ctx context.Context, app, user, id string) error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	tx, err := ss.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM knowledge_sources WHERE app_name=? AND user_id=? AND item_id=?`, app, user, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM knowledge_items WHERE app_name=? AND user_id=? AND id=?`, app, user, id); err != nil {
		return err
	}
	return tx.Commit()
}

// searchPrefilterCap 候选召回硬上限：打分在服务层，预筛只决定进入打分池的规模。
const searchPrefilterCap = 50

// likePattern 生成含转义的 LIKE 模式（ESCAPE '\'）：关键词中的 %、_、\ 按字面匹配。
func likePattern(kw string) string {
	return "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(kw) + "%"
}

// SearchKnowledgeWithSources 关键词候选召回（M3.1）：任一关键词命中
// title/ai_summary/user_understanding 或来源 quote_text/source_title 的
// active 条目（(app,user) 范围隔离）。来源以 EXISTS 参与 WHERE（命中判定
// 覆盖来源级字段），命中条目的全部来源一并回填供服务层打分；keywords 为空
// 直接返回空切片。检索条件来源自知识服务层（不涉及会话事件，与 recall 分离）。
func (ss *sqliteStore) SearchKnowledgeWithSources(ctx context.Context, app, user string, keywords []string, limit int) ([]*storage.KnowledgeHit, error) {
	kws := make([]string, 0, len(keywords))
	for _, kw := range keywords {
		if kw = strings.TrimSpace(kw); kw != "" {
			kws = append(kws, kw)
		}
	}
	if len(kws) == 0 {
		return []*storage.KnowledgeHit{}, nil
	}
	if limit <= 0 {
		limit = searchPrefilterCap
	}
	if limit > searchPrefilterCap {
		limit = searchPrefilterCap
	}

	// OR 链：items 三字段 + sources 两字段（EXISTS），
	// 逐 (关键词, 字段) 展开占位符，与参数严格对齐。
	var conds []string
	args := []any{app, user}
	itemCols := []string{"title", "ai_summary", "user_understanding"}
	for _, kw := range kws {
		pat := likePattern(kw)
		for _, col := range itemCols {
			conds = append(conds, "ki."+col+` LIKE ? ESCAPE '\'`)
			args = append(args, pat)
		}
		conds = append(conds,
			`EXISTS(SELECT 1 FROM knowledge_sources ks WHERE ks.item_id=ki.id AND (ks.quote_text LIKE ? ESCAPE '\' OR ks.source_title LIKE ? ESCAPE '\'))`)
		args = append(args, pat, pat)
	}

	query := `SELECT ` + knowledgeItemCols + ` FROM knowledge_items ki
		WHERE ki.app_name=? AND ki.user_id=? AND ki.status='active'
		  AND (` + strings.Join(conds, " OR ") + `)
		ORDER BY updated_at DESC, rowid DESC LIMIT ?`
	args = append(args, limit)

	ss.mu.Lock()
	defer ss.mu.Unlock()
	rows, err := ss.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var items []*storage.KnowledgeItem
	ids := map[string]bool{}
	for rows.Next() {
		it, err := scanKnowledgeItem(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, it)
		ids[it.ID] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(items) == 0 {
		return []*storage.KnowledgeHit{}, nil
	}

	// 批量回填命中条目的全部来源（单查 IN 列表，避免 N+1）
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	sargs := make([]any, 0, len(ids)+2)
	sargs = append(sargs, app, user)
	for _, it := range items {
		sargs = append(sargs, it.ID)
	}
	srows, err := ss.db.QueryContext(ctx,
		`SELECT `+knowledgeSourceCols+` FROM knowledge_sources
		 WHERE app_name=? AND user_id=? AND item_id IN (`+ph+`) ORDER BY created_at, rowid`, sargs...)
	if err != nil {
		return nil, err
	}
	defer srows.Close()
	sourcesByItem := map[string][]*storage.KnowledgeSource{}
	for srows.Next() {
		src, err := scanKnowledgeSource(srows)
		if err != nil {
			return nil, err
		}
		sourcesByItem[src.ItemID] = append(sourcesByItem[src.ItemID], src)
	}
	if err := srows.Err(); err != nil {
		return nil, err
	}
	out := make([]*storage.KnowledgeHit, 0, len(items))
	for _, it := range items {
		out = append(out, &storage.KnowledgeHit{Item: it, Sources: sourcesByItem[it.ID]})
	}
	return out, nil
}
