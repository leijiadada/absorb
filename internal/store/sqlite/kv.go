package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"absorb-agent/internal/store/storage"

	"trpc.group/trpc-go/trpc-agent-go/session"
)

func (ss *sqliteStore) UpdateAppState(ctx context.Context, appName string, state session.StateMap) error {
	return ss.updateStates(ctx, "app_states", appName, "", state)
}
func (ss *sqliteStore) ListAppStates(ctx context.Context, appName string) (session.StateMap, error) {
	return ss.listStatesLocked(ctx, "app_states", appName, "")
}
func (ss *sqliteStore) DeleteAppState(ctx context.Context, appName, key string) error {
	return ss.deleteState(ctx, "app_states", appName, "", key)
}
func (ss *sqliteStore) UpdateUserState(ctx context.Context, userKey session.UserKey, state session.StateMap) error {
	return ss.updateStates(ctx, "user_states", userKey.AppName, userKey.UserID, state)
}
func (ss *sqliteStore) ListUserStates(ctx context.Context, userKey session.UserKey) (session.StateMap, error) {
	return ss.listStatesLocked(ctx, "user_states", userKey.AppName, userKey.UserID)
}
func (ss *sqliteStore) DeleteUserState(ctx context.Context, userKey session.UserKey, key string) error {
	return ss.deleteState(ctx, "user_states", userKey.AppName, userKey.UserID, key)
}

// updateStates 对 state 里的每个 key 做 upsert。
// table="app_states" 时 userID 忽略；table="user_states" 时按 (app,user) 定位。
func (ss *sqliteStore) updateStates(ctx context.Context, table, appName, userID string, state session.StateMap) error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	for k, v := range state {
		var err error
		if table == "app_states" {
			_, err = ss.db.ExecContext(ctx,
				`INSERT OR REPLACE INTO app_states(app_name, key, value) VALUES(?,?,?)`,
				appName, k, string(v))
		} else {
			_, err = ss.db.ExecContext(ctx,
				`INSERT OR REPLACE INTO user_states(app_name, user_id, key, value) VALUES(?,?,?,?)`,
				appName, userID, k, string(v))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// listStatesLocked 读出某 app（+user）下的全部 key/value，须持锁调用。
func (ss *sqliteStore) listStatesLocked(ctx context.Context, table, appName, userID string) (session.StateMap, error) {
	var rows *sql.Rows
	var err error
	if table == "app_states" {
		rows, err = ss.db.QueryContext(ctx, `SELECT key, value FROM app_states WHERE app_name=?`, appName)
	} else {
		rows, err = ss.db.QueryContext(ctx, `SELECT key, value FROM user_states WHERE app_name=? AND user_id=?`, appName, userID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := session.StateMap{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = []byte(v)
	}
	return out, rows.Err()
}

func (ss *sqliteStore) deleteState(ctx context.Context, table, appName, userID, key string) error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if table == "app_states" {
		_, err := ss.db.ExecContext(ctx, `DELETE FROM app_states WHERE app_name=? AND key=?`, appName, key)
		return err
	}
	_, err := ss.db.ExecContext(ctx, `DELETE FROM user_states WHERE app_name=? AND user_id=? AND key=?`, appName, userID, key)
	return err
}

// SavePageText 保存（覆盖）某 scope 的页面结构化文本缓存（expand_context 本地
// 检索用）。scopeID 为页面 URL（内部规范化），空串表示无 scope 的会话级缓存；
// 同会话多页面各存一行，互不覆盖。pageTitle 顺带落库，供 scope 卡片使用。
func (ss *sqliteStore) SavePageText(ctx context.Context, app, user, sessionID, scopeID, pageTitle, pageText string) error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	_, err := ss.db.ExecContext(ctx,
		`INSERT INTO page_texts (app_name, user_id, session_id, scope_id, page_title, page_text, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(app_name, user_id, session_id, scope_id)
		 DO UPDATE SET page_title = excluded.page_title, page_text = excluded.page_text, updated_at = excluded.updated_at`,
		app, user, sessionID, storage.NormalizeScopeID(scopeID), pageTitle, pageText, time.Now().Unix())
	return err
}

// LoadPageText 读取某 scope 的页面文本缓存；无缓存返回空串（宁缺勿错：
// scope 查无即空，不回退到其他页面的缓存）。
func (ss *sqliteStore) LoadPageText(ctx context.Context, app, user, sessionID, scopeID string) (string, error) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	var pageText string
	err := ss.db.QueryRowContext(ctx,
		`SELECT page_text FROM page_texts WHERE app_name=? AND user_id=? AND session_id=? AND scope_id=?`,
		app, user, sessionID, storage.NormalizeScopeID(scopeID)).Scan(&pageText)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return pageText, nil
}
