package sqlite

import (
	"context"
	"encoding/binary"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"

	"absorb-agent/internal/store/storage"
)

// 文档 chunk 向量（M3 语义召回）：float32 BLOB 编解码 + 文档级原子替换 +
// 查询期全量加载（内存暴力余弦，万级 chunk 足够，无需向量索引）。

// ReplaceDocumentVectors 单事务整体替换文档向量：删旧行 → 按 chunk_id 插新行。
// 与 ReplaceDocumentIndex 对称——chunks 重建后 chunk_id 全换，旧向量成为孤儿，
// 必须整体替换。vectors 的 ChunkIndex 用于 JOIN 排序兜底。
func (ss *sqliteStore) ReplaceDocumentVectors(ctx context.Context, app, user, documentID, model string, vectors []*storage.ChunkVectorRow) error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	tx, err := ss.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM document_chunk_vectors WHERE app_name=? AND user_id=? AND document_id=? AND model=?`,
		app, user, documentID, model); err != nil {
		return err
	}
	now := time.Now().Unix()
	for _, v := range vectors {
		if v.Vector == nil {
			continue
		}
		if v.ChunkID == "" {
			v.ChunkID = uuid.NewString()
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO document_chunk_vectors
				(chunk_id, app_name, user_id, document_id, chunk_index, dims, model, vector, created_at)
			 VALUES (?,?,?,?,?,?,?,?,?)`,
			v.ChunkID, app, user, documentID, v.ChunkIndex, len(v.Vector), model,
			packVector(v.Vector), now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LoadDocumentVectors 读取文档级向量（按 model 过滤）。
func (ss *sqliteStore) LoadDocumentVectors(ctx context.Context, app, user, documentID, model string) ([]storage.ChunkVectorRow, error) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	rows, err := ss.db.QueryContext(ctx,
		`SELECT chunk_id, document_id, chunk_index, dims, vector, created_at, model
		 FROM document_chunk_vectors
		 WHERE app_name=? AND user_id=? AND document_id=? AND model=?
		 ORDER BY chunk_index`, app, user, documentID, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storage.ChunkVectorRow
	for rows.Next() {
		var v storage.ChunkVectorRow
		var created int64
		var blob []byte
		if err := rows.Scan(&v.ChunkID, &v.DocumentID, &v.ChunkIndex, &v.Dims, &blob, &created, &v.Model); err != nil {
			return nil, err
		}
		v.Vector = unpackVector(blob, v.Dims)
		v.CreatedAt = time.Unix(created, 0)
		out = append(out, v)
	}
	return out, rows.Err()
}

// LoadVectorHits 查询期加载：当前用户（可选范围限定）全部有效向量，JOIN
// chunks/documents 直接带回原文与标题——融合层无需二次查询。向量数与命中数
// 同源同序（按 rowid 稳定对齐），余弦得分按位次回填。
func (ss *sqliteStore) LoadVectorHits(ctx context.Context, app, user, model string, documentIDs []string) ([]storage.DocumentChunkHit, []storage.ChunkVectorRow, error) {
	q := `SELECT v.chunk_id, v.document_id, c.chunk_index, c.section_path,
			c.content, c.start_offset, c.end_offset, d.title,
			v.dims, v.vector, v.created_at
		 FROM document_chunk_vectors v
		 JOIN document_chunks c ON c.id = v.chunk_id
		 JOIN documents d
		   ON d.app_name = c.app_name AND d.user_id = c.user_id AND d.id = c.document_id
		 WHERE v.app_name=? AND v.user_id=? AND v.model=?`
	args := []any{app, user, model}
	if len(documentIDs) > 0 {
		q += ` AND v.document_id IN (` + strings.Repeat("?,", len(documentIDs)-1) + `?)`
		for _, id := range documentIDs {
			args = append(args, id)
		}
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	rows, err := ss.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var hits []storage.DocumentChunkHit
	var vectors []storage.ChunkVectorRow
	for rows.Next() {
		var h storage.DocumentChunkHit
		var v storage.ChunkVectorRow
		var created int64
		var blob []byte
		if err := rows.Scan(&v.ChunkID, &v.DocumentID, &h.ChunkIndex, &h.SectionPath,
			&h.Content, &h.StartOffset, &h.EndOffset, &h.Title,
			&v.Dims, &blob, &created); err != nil {
			return nil, nil, err
		}
		h.DocumentID = v.DocumentID
		v.Vector = unpackVector(blob, v.Dims)
		v.CreatedAt = time.Unix(created, 0)
		hits = append(hits, h)
		vectors = append(vectors, v)
	}
	return hits, vectors, rows.Err()
}

// packVector float32 切片 → little-endian BLOB。
func packVector(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

// unpackVector BLOB → float32 切片（长度与 dims 不符视为损坏行，返回 nil 跳过）。
func unpackVector(b []byte, dims int) []float32 {
	if len(b) != 4*dims {
		return nil
	}
	out := make([]float32, dims)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out
}
