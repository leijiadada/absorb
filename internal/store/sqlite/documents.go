package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"

	"absorb-agent/internal/store/storage"
)

// 文档聚合（M2.5）：documents 表按 URL 锚定原始资料。与知识表同约定——
// 无外键、时间戳由本层填充、唯一索引即聚合键。

const documentCols = `id, app_name, user_id, url, title, summary_text, summary_session_id,
	summary_event_id, created_at, updated_at`

// scanDocument 读出一行 documents。
func scanDocument(sc scanner) (*storage.Document, error) {
	var d storage.Document
	var created, updated int64
	if err := sc.Scan(&d.ID, &d.AppName, &d.UserID, &d.URL, &d.Title, &d.SummaryText,
		&d.SummarySessionID, &d.SummaryEventID, &created, &updated); err != nil {
		return nil, err
	}
	d.CreatedAt = time.Unix(created, 0)
	d.UpdatedAt = time.Unix(updated, 0)
	return &d, nil
}

// UpsertDocument 同 (app,user,url) 取或建文档锚点：INSERT OR IGNORE 未命中
// （唯一索引拦下重复 URL）时回读既有行；title 变化时刷新。
func (ss *sqliteStore) UpsertDocument(ctx context.Context, app, user, url, title string) (*storage.Document, error) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	now := time.Now().Unix()
	res, err := ss.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO documents(`+documentCols+`)
		 VALUES(?,?,?,?,?,?,?,?,?,?)`,
		uuid.NewString(), app, user, url, title, "", "", "", now, now)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 已存在：回读，并在标题变化时刷新（页面改标题是常态，锚点跟最新）
		if _, err := ss.db.ExecContext(ctx,
			`UPDATE documents SET title=?, updated_at=? WHERE app_name=? AND user_id=? AND url=? AND title<>?`,
			title, now, app, user, url, title); err != nil {
			return nil, err
		}
	}
	row := ss.db.QueryRowContext(ctx,
		`SELECT `+documentCols+` FROM documents WHERE app_name=? AND user_id=? AND url=?`,
		app, user, url)
	return scanDocument(row)
}

// UpdateDocumentSummary 覆盖写阅读摘要三列（设计文档层次A：AI 产物整体替换），
// 刷新 updated_at。幂等判断（summary_event_id 比对）由服务层在写前完成。
func (ss *sqliteStore) UpdateDocumentSummary(ctx context.Context, app, user, docID, summaryText, sessionID, eventID string) error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	res, err := ss.db.ExecContext(ctx,
		`UPDATE documents SET summary_text=?, summary_session_id=?, summary_event_id=?, updated_at=?
		 WHERE app_name=? AND user_id=? AND id=?`,
		summaryText, sessionID, eventID, time.Now().Unix(), app, user, docID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("documents: row not found")
	}
	return nil
}

// SaveDocumentContent 覆盖写文档提取正文（document_contents，PDF 解读 PR2）：
// 同 (app,user,url) 重新准备即整体刷新。FetchedAt 由调用方传入（获取事实时刻），
// updated_at 由本层填充。
func (ss *sqliteStore) SaveDocumentContent(ctx context.Context, app, user string, dc *storage.DocumentContent) error {
	if dc == nil {
		return errors.New("documents: nil content")
	}
	now := time.Now().Unix()
	ss.mu.Lock()
	defer ss.mu.Unlock()
	_, err := ss.db.ExecContext(ctx,
		`INSERT INTO document_contents
			(app_name, user_id, url, content_text, char_count, content_type, byte_size,
			 content_hash, extraction_status, extraction_version, truncated, fetched_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(app_name, user_id, url) DO UPDATE SET
			content_text=excluded.content_text, char_count=excluded.char_count,
			content_type=excluded.content_type, byte_size=excluded.byte_size,
			content_hash=excluded.content_hash, extraction_status=excluded.extraction_status,
			extraction_version=excluded.extraction_version, truncated=excluded.truncated,
			fetched_at=excluded.fetched_at, updated_at=excluded.updated_at`,
		app, user, dc.URL, dc.ContentText, dc.CharCount, dc.ContentType, dc.ByteSize,
		dc.ContentHash, dc.ExtractionStatus, dc.ExtractionVersion, dc.Truncated,
		dc.FetchedAt.Unix(), now)
	return err
}

// LoadDocumentContent 读取文档提取正文；无缓存返回 (nil, nil)（宁缺勿错，
// 调用方按未准备处理，不回退其他文档）。
func (ss *sqliteStore) LoadDocumentContent(ctx context.Context, app, user, url string) (*storage.DocumentContent, error) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	var dc storage.DocumentContent
	var fetched, updated int64
	err := ss.db.QueryRowContext(ctx,
		`SELECT url, content_text, char_count, content_type, byte_size, content_hash,
			extraction_status, extraction_version, truncated, fetched_at, updated_at
		 FROM document_contents WHERE app_name=? AND user_id=? AND url=?`,
		app, user, url).Scan(&dc.URL, &dc.ContentText, &dc.CharCount, &dc.ContentType, &dc.ByteSize,
		&dc.ContentHash, &dc.ExtractionStatus, &dc.ExtractionVersion, &dc.Truncated, &fetched, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	dc.FetchedAt = time.Unix(fetched, 0)
	dc.UpdatedAt = time.Unix(updated, 0)
	return &dc, nil
}
