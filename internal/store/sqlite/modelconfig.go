package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"absorb-agent/internal/store/storage"
)

// keyDefaultModel app_states 中存默认模型 ID 的 KV 键。
const keyDefaultModel = "default_model_id"

// scanner 统一 *sql.Row / *sql.Rows 的 Scan 接口。
type scanner interface{ Scan(dest ...any) error }

// scanModelConfig 读出一行 model_configs。
func scanModelConfig(sc scanner) (*storage.ModelConfig, error) {
	var mc storage.ModelConfig
	var fullURL, imageSupport int64
	var created, updated int64
	var temp sql.NullFloat64
	var maxTok, toolRounds sql.NullInt64
	if err := sc.Scan(&mc.ID, &mc.Provider, &mc.DisplayName, &mc.ModelID, &mc.APIFormat,
		&mc.BaseURL, &fullURL, &mc.APIKey, &temp, &maxTok, &mc.Thinking, &toolRounds, &imageSupport, &created, &updated); err != nil {
		return nil, err
	}
	mc.FullURL = fullURL != 0
	mc.ImageSupport = imageSupport != 0
	if temp.Valid {
		v := temp.Float64
		mc.Temperature = &v
	}
	if maxTok.Valid {
		v := int(maxTok.Int64)
		mc.MaxTokens = &v
	}
	if toolRounds.Valid {
		v := int(toolRounds.Int64)
		mc.ToolRounds = &v
	}
	mc.CreatedAt = time.Unix(created, 0)
	mc.UpdatedAt = time.Unix(updated, 0)
	return &mc, nil
}

// ListModelConfigs 全部模型配置（按创建时间升序，先添加的排前）。
func (ss *sqliteStore) ListModelConfigs(ctx context.Context) ([]storage.ModelConfig, error) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	rows, err := ss.db.QueryContext(ctx,
		`SELECT id, provider, display_name, model_id, api_format, base_url, full_url, api_key,
		        temperature, max_tokens, thinking, tool_rounds, image_support, created_at, updated_at
		 FROM model_configs ORDER BY created_at, rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storage.ModelConfig
	for rows.Next() {
		mc, err := scanModelConfig(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *mc)
	}
	return out, rows.Err()
}

// getModelConfigLocked 已持锁的单条读取。
func (ss *sqliteStore) getModelConfigLocked(ctx context.Context, id string) (*storage.ModelConfig, error) {
	row := ss.db.QueryRowContext(ctx,
		`SELECT id, provider, display_name, model_id, api_format, base_url, full_url, api_key,
		        temperature, max_tokens, thinking, tool_rounds, image_support, created_at, updated_at
		 FROM model_configs WHERE id=?`, id)
	mc, err := scanModelConfig(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return mc, nil
}

// GetModelConfig 按 ID 读取单条；不存在返回 (nil, nil)。
func (ss *sqliteStore) GetModelConfig(ctx context.Context, id string) (*storage.ModelConfig, error) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.getModelConfigLocked(ctx, id)
}

// CreateModelConfig 新增一条（ID/字段由调用方填充）。
func (ss *sqliteStore) CreateModelConfig(ctx context.Context, mc *storage.ModelConfig) error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	now := time.Now().Unix()
	_, err := ss.db.ExecContext(ctx,
		`INSERT INTO model_configs(id, provider, display_name, model_id, api_format, base_url, full_url, api_key, temperature, max_tokens, thinking, tool_rounds, image_support, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		mc.ID, mc.Provider, mc.DisplayName, mc.ModelID, mc.APIFormat, mc.BaseURL, boolInt(mc.FullURL),
		mc.APIKey, nullFloat(mc.Temperature), nullInt(mc.MaxTokens), mc.Thinking, nullInt(mc.ToolRounds), boolInt(mc.ImageSupport), now, now)
	return err
}

// UpdateModelConfig 整行更新（Key 留空保留原值的语义由调用方处理），刷新 updated_at。
func (ss *sqliteStore) UpdateModelConfig(ctx context.Context, mc *storage.ModelConfig) error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	_, err := ss.db.ExecContext(ctx,
		`UPDATE model_configs SET provider=?, display_name=?, model_id=?, api_format=?, base_url=?,
		        full_url=?, api_key=?, temperature=?, max_tokens=?, thinking=?, tool_rounds=?, image_support=?, updated_at=?
		 WHERE id=?`,
		mc.Provider, mc.DisplayName, mc.ModelID, mc.APIFormat, mc.BaseURL, boolInt(mc.FullURL),
		mc.APIKey, nullFloat(mc.Temperature), nullInt(mc.MaxTokens), mc.Thinking, nullInt(mc.ToolRounds), boolInt(mc.ImageSupport),
		time.Now().Unix(), mc.ID)
	return err
}

// DeleteModelConfig 删除单条。
func (ss *sqliteStore) DeleteModelConfig(ctx context.Context, id string) error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	_, err := ss.db.ExecContext(ctx, `DELETE FROM model_configs WHERE id=?`, id)
	return err
}

// GetDefaultModelConfig 读默认模型配置（app_states KV → model_configs 行）。
// 无默认或 KV 悬空（行已被删）返回 (nil, nil)。
func (ss *sqliteStore) GetDefaultModelConfig(ctx context.Context, app string) (*storage.ModelConfig, error) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	var id string
	err := ss.db.QueryRowContext(ctx,
		`SELECT value FROM app_states WHERE app_name=? AND key=?`, app, keyDefaultModel).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ss.getModelConfigLocked(ctx, id)
}

// GetDefaultModelID 读默认模型 ID（无默认返回空串）。
func (ss *sqliteStore) GetDefaultModelID(ctx context.Context, app string) (string, error) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	var id string
	err := ss.db.QueryRowContext(ctx,
		`SELECT value FROM app_states WHERE app_name=? AND key=?`, app, keyDefaultModel).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return id, nil
}

// SetDefaultModelID 设置默认模型 KV。
func (ss *sqliteStore) SetDefaultModelID(ctx context.Context, app, id string) error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	_, err := ss.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO app_states(app_name, key, value) VALUES(?,?,?)`, app, keyDefaultModel, id)
	return err
}

// ClearDefaultModelID 清除默认模型 KV（列表清空时回"未配置"态）。
func (ss *sqliteStore) ClearDefaultModelID(ctx context.Context, app string) error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	_, err := ss.db.ExecContext(ctx,
		`DELETE FROM app_states WHERE app_name=? AND key=?`, app, keyDefaultModel)
	return err
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func nullFloat(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}
