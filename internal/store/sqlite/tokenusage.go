package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"absorb-agent/internal/store/storage"
)

// token_usage 流水表直查实现（telemetry 第一批）：只增不改，一次模型调用一行。
// 与 ext_session_mirror 等表同约定——无外键、时间戳 Unix 秒；聚合/查询留给
// 后续批次（第一批只用于日志），写入失败由调用方（tokentracker sink 装配处）
// 记日志降级，不影响主链路。
// telemetry 第二批新增 TokenUsageStats 聚合查询（展示层数据源）。

// InsertTokenUsage 插入一条 token 用量流水（tokentracker sink 装配处调用）。
func (ss *sqliteStore) InsertTokenUsage(ctx context.Context, rec storage.TokenUsageRecord) error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	_, err := ss.db.ExecContext(ctx,
		`INSERT INTO token_usage(ts, kind, model, prompt_tokens, completion_tokens, reasoning_tokens, total_tokens, cached_tokens)
		 VALUES(?,?,?,?,?,?,?,?)`,
		rec.TS.Unix(), rec.Kind, rec.Model,
		rec.PromptTokens, rec.CompletionTokens, rec.ReasoningTokens,
		rec.TotalTokens, rec.CachedTokens)
	return err
}

// usageAggCols 聚合列清单（调用次数 + 各用量列求和）：today/total/bykind 三个查询共用。
// 空表时 SUM 返回 NULL，统一 COALESCE 为 0。
const usageAggCols = `COUNT(*), COALESCE(SUM(total_tokens),0), COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0), COALESCE(SUM(cached_tokens),0)`

// scanUsageTotals 读取单行聚合结果为 UsageTotals（列序对应 usageAggCols）。
func scanUsageTotals(row *sql.Row) (storage.UsageTotals, error) {
	var t storage.UsageTotals
	err := row.Scan(&t.Calls, &t.TotalTokens, &t.PromptTokens, &t.CompletionTokens, &t.CachedTokens)
	return t, err
}

// TokenUsageStats 聚合 token 用量统计（telemetry 第二批，桌面端"Token 用量"卡片数据源）：
// 今日（本地当日 0 点起）、全期合计、按 kind 的全期分布与今日缓存命中率。
// 三次轻量聚合查询（ts 列有索引），经全局写锁串行执行——展示是旁路链路，
// 失败由调用方（绑定层）记日志降级，不影响页面。
func (ss *sqliteStore) TokenUsageStats(ctx context.Context) (storage.TokenUsageStatsResult, error) {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	res := storage.TokenUsageStatsResult{ByKind: map[string]storage.UsageTotals{}}

	// 今日边界：本地当日 0 点的 Unix 秒（写入侧 ts 也是本地时间取 Unix，口径一致）
	n := time.Now().In(time.Local)
	midnight := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.Local)

	var err error
	if res.Today, err = scanUsageTotals(ss.db.QueryRowContext(ctx,
		`SELECT `+usageAggCols+` FROM token_usage WHERE ts >= ?`, midnight.Unix())); err != nil {
		return res, fmt.Errorf("token usage today: %w", err)
	}
	if res.Total, err = scanUsageTotals(ss.db.QueryRowContext(ctx,
		`SELECT `+usageAggCols+` FROM token_usage`)); err != nil {
		return res, fmt.Errorf("token usage total: %w", err)
	}

	rows, err := ss.db.QueryContext(ctx,
		`SELECT kind, `+usageAggCols+` FROM token_usage GROUP BY kind`)
	if err != nil {
		return res, fmt.Errorf("token usage by kind: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var t storage.UsageTotals
		if err := rows.Scan(&kind, &t.Calls, &t.TotalTokens, &t.PromptTokens, &t.CompletionTokens, &t.CachedTokens); err != nil {
			return res, err
		}
		res.ByKind[kind] = t
	}
	if err := rows.Err(); err != nil {
		return res, err
	}

	// 今日缓存命中率：cached/prompt（0-1）；今日无 prompt 调用时为 0（除零安全）
	if res.Today.PromptTokens > 0 {
		res.CacheHitRate = float64(res.Today.CachedTokens) / float64(res.Today.PromptTokens)
	}
	return res, nil
}
