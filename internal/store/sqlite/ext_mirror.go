package sqlite

import (
	"context"
	"database/sql"
	"time"

	"absorb-agent/internal/store/storage"
)

// 扩展端会话镜像（design/ext-session-sync-design.md）：ext_session_mirror 表的
// 直查实现。与知识/文档表同约定——无外键、时间戳 Unix 秒、主键即聚合键。
// Data 存扩展原生会话对象 JSON 原文，本层不解析不重组（镜像而非转换）。

const extMirrorCols = `session_id, title, msg_count, data, created_at, updated_at`

// scanExtMirrorIndex 读一行索引条目（不含 data 列）。
func scanExtMirrorIndex(sc scanner) (*storage.ExtMirrorIndex, error) {
	var it storage.ExtMirrorIndex
	var created, updated int64
	if err := sc.Scan(&it.SessionID, &it.Title, &it.MsgCount, &created, &updated); err != nil {
		return nil, err
	}
	it.CreatedAt = time.Unix(created, 0)
	it.UpdatedAt = time.Unix(updated, 0)
	return &it, nil
}

// UpsertExtMirror 幂等覆盖写一条镜像。冲突时保留既有 created_at（首见时间不因
// 重复上传漂移），刷新 title/msg_count/data/updated_at。
func (ss *sqliteStore) UpsertExtMirror(ctx context.Context, app, user, sessionID, title string, msgCount int, data string, createdAt, updatedAt time.Time) error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	_, err := ss.db.ExecContext(ctx,
		`INSERT INTO ext_session_mirror(app_name, user_id, session_id, title, msg_count, data, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?)
		 ON CONFLICT(app_name, user_id, session_id) DO UPDATE SET
		   title=excluded.title, msg_count=excluded.msg_count, data=excluded.data,
		   updated_at=excluded.updated_at`,
		app, user, sessionID, title, msgCount, data, createdAt.Unix(), updatedAt.Unix())
	return err
}

// ListExtMirrors 返回 (app,user) 范围内全部镜像索引，按 updated_at 倒序。
func (ss *sqliteStore) ListExtMirrors(ctx context.Context, app, user string) ([]*storage.ExtMirrorIndex, error) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	rows, err := ss.db.QueryContext(ctx,
		`SELECT session_id, title, msg_count, created_at, updated_at
		 FROM ext_session_mirror WHERE app_name=? AND user_id=?
		 ORDER BY updated_at DESC`,
		app, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*storage.ExtMirrorIndex
	for rows.Next() {
		it, err := scanExtMirrorIndex(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// GetExtMirror 读单条镜像完整记录；不存在返回 (nil, nil)。
func (ss *sqliteStore) GetExtMirror(ctx context.Context, app, user, sessionID string) (*storage.ExtMirrorRecord, error) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	var rec storage.ExtMirrorRecord
	var created, updated int64
	err := ss.db.QueryRowContext(ctx,
		`SELECT `+extMirrorCols+` FROM ext_session_mirror
		 WHERE app_name=? AND user_id=? AND session_id=?`,
		app, user, sessionID).
		Scan(&rec.SessionID, &rec.Title, &rec.MsgCount, &rec.Data, &created, &updated)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rec.CreatedAt = time.Unix(created, 0)
	rec.UpdatedAt = time.Unix(updated, 0)
	return &rec, nil
}
