package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"absorb-agent/internal/store/storage"
)

// 文档检索索引（M1）：document_chunks + document_chunks_fts + document_index_state。
// chunk/FTS 是 document_contents 的派生数据，唯一写入口是 ReplaceDocumentIndex
// （单事务整体替换，失败回滚不留新旧混杂中间态）。

// ReplaceDocumentIndex 原子替换文档索引：事务内删旧 chunks+FTS 行 → 插新 chunks
// → 写 FTS（title 取 documents.title 参与加权）→ 覆盖 index_state。chunks 为 nil
// 时不触碰既有索引，仅覆盖 index_state（构建失败标记路径：保留旧索引、明确待重建）。
// chunks 替换时同事务作废该文档全部向量（chunk_id 全换，任何模型的旧向量均成孤儿），
// 向量域状态随 state 写入重置，由 embedChunks 重建。
func (ss *sqliteStore) ReplaceDocumentIndex(ctx context.Context, app, user, documentID string, chunks []*storage.DocumentChunk, state *storage.DocumentIndexState) error {
	if state == nil {
		return errors.New("documentindex: nil index state")
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	tx, err := ss.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if len(chunks) > 0 {
		// 旧 FTS 行按 chunk_id 删除（先取 ids，删 chunks 后无法再反查）
		ids, err := selectChunkIDs(ctx, tx, app, user, documentID)
		if err != nil {
			return err
		}
		for i := 0; i < len(ids); i += 500 { // SQLite 变量数上限防御性分批
			end := i + 500
			if end > len(ids) {
				end = len(ids)
			}
			batch := ids[i:end]
			q := `DELETE FROM document_chunks_fts WHERE chunk_id IN (` +
				strings.Repeat("?,", len(batch)-1) + `?)`
			args := make([]any, len(batch))
			for j, id := range batch {
				args[j] = id
			}
			if _, err := tx.ExecContext(ctx, q, args...); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM document_chunks WHERE app_name=? AND user_id=? AND document_id=?`,
			app, user, documentID); err != nil {
			return err
		}
		// 向量以 chunk_id 为主键关联：chunks 全换即全部作废（含历史模型残留行）
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM document_chunk_vectors WHERE app_name=? AND user_id=? AND document_id=?`,
			app, user, documentID); err != nil {
			return err
		}
		title := ""
		if err := tx.QueryRowContext(ctx,
			`SELECT title FROM documents WHERE app_name=? AND user_id=? AND id=?`,
			app, user, documentID).Scan(&title); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		now := time.Now().Unix()
		for _, c := range chunks {
			if c.ID == "" {
				c.ID = uuid.NewString()
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO document_chunks(id, app_name, user_id, document_id, chunk_index,
					section_path, content, search_text, start_offset, end_offset, token_count,
					content_hash, chunker_version, tokenizer_version, created_at)
				 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				c.ID, app, user, documentID, c.ChunkIndex, c.SectionPath, c.Content, c.SearchText,
				c.StartOffset, c.EndOffset, c.TokenCount, c.ContentHash, c.ChunkerVersion,
				c.TokenizerVersion, now); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO document_chunks_fts(chunk_id, title, section_path, search_text)
				 VALUES (?,?,?,?)`,
				c.ID, title, c.SectionPath, c.SearchText); err != nil {
				return err
			}
		}
	}
	if err := upsertIndexState(ctx, tx, app, user, documentID, state); err != nil {
		return err
	}
	return tx.Commit()
}

// LoadDocumentIndexState 读取索引状态；查无返回 (nil, nil)（未建过索引）。
func (ss *sqliteStore) LoadDocumentIndexState(ctx context.Context, app, user, documentID string) (*storage.DocumentIndexState, error) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	var st storage.DocumentIndexState
	var indexedAt int64
	err := ss.db.QueryRowContext(ctx,
		`SELECT document_id, indexed_content_hash, index_status, chunker_version,
			tokenizer_version, chunk_count, indexed_at, error_message,
			vector_model, vector_status, vector_count, vector_error
		 FROM document_index_state WHERE app_name=? AND user_id=? AND document_id=?`,
		app, user, documentID).Scan(&st.DocumentID, &st.IndexedContentHash, &st.IndexStatus,
		&st.ChunkerVersion, &st.TokenizerVersion, &st.ChunkCount, &indexedAt, &st.ErrorMessage,
		&st.VectorModel, &st.VectorStatus, &st.VectorCount, &st.VectorError)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	st.IndexedAt = time.Unix(indexedAt, 0)
	return &st, nil
}

// LoadDocumentChunks 读取文档现有 chunks（chunk_index 升序）：词法索引就绪但
// 向量缺失/换模型时的补建路径用——复用 chunks，不重写词法索引。查无返回空切片。
func (ss *sqliteStore) LoadDocumentChunks(ctx context.Context, app, user, documentID string) ([]*storage.DocumentChunk, error) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	rows, err := ss.db.QueryContext(ctx,
		`SELECT id, document_id, chunk_index, section_path, content, search_text,
			start_offset, end_offset, token_count, content_hash, chunker_version,
			tokenizer_version, created_at
		 FROM document_chunks WHERE app_name=? AND user_id=? AND document_id=?
		 ORDER BY chunk_index`, app, user, documentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*storage.DocumentChunk
	for rows.Next() {
		var c storage.DocumentChunk
		var created int64
		if err := rows.Scan(&c.ID, &c.DocumentID, &c.ChunkIndex, &c.SectionPath, &c.Content,
			&c.SearchText, &c.StartOffset, &c.EndOffset, &c.TokenCount, &c.ContentHash,
			&c.ChunkerVersion, &c.TokenizerVersion, &created); err != nil {
			return nil, err
		}
		c.CreatedAt = time.Unix(created, 0)
		out = append(out, &c)
	}
	return out, rows.Err()
}

// UpdateDocumentVectorState 只覆盖 document_index_state 的 vector_* 四列
// （向量域状态）：embedChunks 成功/失败后调用，不触碰词法状态与 chunks/FTS。
func (ss *sqliteStore) UpdateDocumentVectorState(ctx context.Context, app, user, documentID string, vs storage.DocumentVectorState) error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	_, err := ss.db.ExecContext(ctx,
		`UPDATE document_index_state SET vector_model=?, vector_status=?, vector_count=?, vector_error=?
		 WHERE app_name=? AND user_id=? AND document_id=?`,
		vs.Model, vs.Status, vs.Count, vs.Error, app, user, documentID)
	return err
}

// SearchDocumentChunks FTS5 + bm25 检索：query 为分词后的 MATCH 表达式（调用方
// 经 TextAnalyzer 产出；空查询返回空结果不报错）。documentIDs 为空 = 当前用户
// 全部已索引文档。租户条件（app/user）与范围（documentIDs）在 SQL 层强制，
// 不信任调用方拼接。返回按 bm25 升序（越小越相关）。
func (ss *sqliteStore) SearchDocumentChunks(ctx context.Context, app, user, query string, documentIDs []string, limit int) ([]storage.DocumentChunkHit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 8
	}
	if limit > 50 {
		limit = 50
	}
	q := `SELECT c.document_id, d.title, c.chunk_index, c.section_path,
			c.content, c.start_offset, c.end_offset,
			bm25(document_chunks_fts, 0.0, 4.0, 2.0, 1.0) AS rank
		 FROM document_chunks_fts f
		 JOIN document_chunks c ON c.id = f.chunk_id
		 JOIN documents d
		   ON d.app_name = c.app_name AND d.user_id = c.user_id AND d.id = c.document_id
		 WHERE document_chunks_fts MATCH ?
		   AND c.app_name = ? AND c.user_id = ?`
	args := []any{query, app, user}
	if len(documentIDs) > 0 {
		q += ` AND c.document_id IN (` + strings.Repeat("?,", len(documentIDs)-1) + `?)`
		for _, id := range documentIDs {
			args = append(args, id)
		}
	}
	q += ` ORDER BY rank ASC LIMIT ?`
	args = append(args, limit)

	ss.mu.Lock()
	defer ss.mu.Unlock()
	rows, err := ss.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []storage.DocumentChunkHit
	for rows.Next() {
		var h storage.DocumentChunkHit
		if err := rows.Scan(&h.DocumentID, &h.Title, &h.ChunkIndex, &h.SectionPath,
			&h.Content, &h.StartOffset, &h.EndOffset, &h.Score); err != nil {
			return nil, err
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// selectChunkIDs 取文档现有 chunk id（FTS 清理用）。
func selectChunkIDs(ctx context.Context, tx *sql.Tx, app, user, documentID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM document_chunks WHERE app_name=? AND user_id=? AND document_id=?`,
		app, user, documentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// upsertIndexState 覆盖写索引状态（事务内调用，含向量域四列——chunks 替换或
// 失败标记均重置向量域，由 embedChunks 补建后经 UpdateDocumentVectorState 更新）。
func upsertIndexState(ctx context.Context, tx *sql.Tx, app, user, documentID string, st *storage.DocumentIndexState) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO document_index_state
			(app_name, user_id, document_id, indexed_content_hash, index_status,
			 chunker_version, tokenizer_version, chunk_count, indexed_at, error_message,
			 vector_model, vector_status, vector_count, vector_error)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(app_name, user_id, document_id) DO UPDATE SET
			indexed_content_hash=excluded.indexed_content_hash,
			index_status=excluded.index_status,
			chunker_version=excluded.chunker_version,
			tokenizer_version=excluded.tokenizer_version,
			chunk_count=excluded.chunk_count,
			indexed_at=excluded.indexed_at,
			error_message=excluded.error_message,
			vector_model=excluded.vector_model,
			vector_status=excluded.vector_status,
			vector_count=excluded.vector_count,
			vector_error=excluded.vector_error`,
		app, user, documentID, st.IndexedContentHash, st.IndexStatus,
		st.ChunkerVersion, st.TokenizerVersion, st.ChunkCount,
		st.IndexedAt.Unix(), st.ErrorMessage,
		st.VectorModel, st.VectorStatus, st.VectorCount, st.VectorError)
	return err
}
