// 索引构建（M1）：document_contents 全文 → 分块 → 分词 → 事务内整体替换写入
// document_chunks / document_chunks_fts / document_index_state。
// 索引是派生数据：构建失败不阻塞 prepare（提取是事实、索引是加速器），
// 只标 failed 待重建；重建由下次 prepare 或未来按 index_state 调度。
package document

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
	"unicode/utf8"

	"absorb-agent/internal/log"
	"absorb-agent/internal/retrieval"
	"absorb-agent/internal/store/storage"
)

// WithEmbedder 注入语义向量化接缝（M3 混合检索）：nil = 禁用，词法兜底。
// 由装配层按配置注入（conf.Embedding.APIKey 非空时）。
func (s *Service) WithEmbedder(e retrieval.Embedder) *Service {
	s.embedder = e
	return s
}

// BuildIndex 为已落库正文构建检索索引并整体替换。失败不返回错误（调用方是
// Prepare 主流程）：标 failed、记日志，旧索引保留（宁可用旧索引也不错删）。
// 幂等：index_state 显示 ready 且 content_hash / 切块与分词版本均未变化时跳过
// （重复 prepare 不重复分词——20 万字正文省 1~2s，也是 document_index_state
// 存在的本意：事实数据与派生索引的生命周期分离）。
func (s *Service) BuildIndex(ctx context.Context, documentID string, dc *storage.DocumentContent) {
	if documentID == "" || dc == nil || strings.TrimSpace(dc.ContentText) == "" {
		return
	}
	if st, err := s.st.LoadDocumentIndexState(ctx, s.app, s.user, documentID); err == nil && st != nil &&
		st.IndexStatus == storage.IndexStatusReady &&
		st.IndexedContentHash == dc.ContentHash &&
		st.ChunkerVersion == ChunkerVersion &&
		st.TokenizerVersion == TokenizerVersion {
		return
	}
	start := time.Now()
	state := &storage.DocumentIndexState{
		DocumentID:         documentID,
		IndexedContentHash: dc.ContentHash,
		ChunkerVersion:     ChunkerVersion,
		TokenizerVersion:   TokenizerVersion,
	}
	chunks, err := s.buildChunks(dc.ContentText)
	if err == nil {
		state.ChunkCount = len(chunks)
		state.IndexStatus = storage.IndexStatusReady
		state.IndexedAt = time.Now()
		err = s.st.ReplaceDocumentIndex(ctx, s.app, s.user, documentID, chunks, state)
	}
	if err != nil {
		// 失败标记走同一事务接口（chunks=nil 不触碰既有索引），记原因待重建
		state.IndexStatus = storage.IndexStatusFailed
		state.ErrorMessage = err.Error()
		state.ChunkCount = 0
		state.IndexedAt = time.Now()
		if markErr := s.st.ReplaceDocumentIndex(ctx, s.app, s.user, documentID, nil, state); markErr != nil {
			log.Warnf("[Absorb] document index mark-failed also failed (doc=%s): %v", documentID, markErr)
		}
		log.Warnf("[Absorb] document index build failed (doc=%s, marked pending-rebuild): %v", documentID, err)
		return
	}
	log.Infof("[Absorb] document index built | doc=%s chunks=%d chars=%d dur=%s",
		documentID, len(chunks), dc.CharCount, time.Since(start).Round(time.Millisecond))
	s.embedChunks(ctx, documentID, chunks)
}

// embedChunks 索引期语义向量化（M3）：chunks 已替换成功后按 chunk_index 批量
// 向量入库。向量是派生数据的派生数据——失败只告警不回滚（词法路完整可用），
// 重建由下次 prepare 触发；chunk_id 与本次写入一致，余弦路按 model 过滤加载。
func (s *Service) embedChunks(ctx context.Context, documentID string, chunks []*storage.DocumentChunk) {
	if s.embedder == nil || len(chunks) == 0 {
		return
	}
	start := time.Now()
	texts := make([]string, len(chunks))
	rows := make([]*storage.ChunkVectorRow, len(chunks))
	for i, c := range chunks {
		texts[i] = c.Content
		rows[i] = &storage.ChunkVectorRow{ChunkIndex: c.ChunkIndex}
	}
	vecs, err := s.embedder.EmbedDocuments(ctx, texts)
	if err != nil {
		log.Warnf("[Absorb] chunk embedding failed (doc=%s, lexical-only until next rebuild): %v", documentID, err)
		return
	}
	for i, v := range vecs {
		rows[i].ChunkID = chunks[i].ID
		rows[i].DocumentID = documentID
		rows[i].Dims = len(v)
		rows[i].Model = s.embedder.Model()
		rows[i].Vector = v
	}
	if err := s.st.ReplaceDocumentVectors(ctx, s.app, s.user, documentID, s.embedder.Model(), rows); err != nil {
		log.Warnf("[Absorb] chunk vector persist failed (doc=%s): %v", documentID, err)
		return
	}
	log.Infof("[Absorb] chunk vectors built | doc=%s count=%d model=%s dur=%s",
		documentID, len(rows), s.embedder.Model(), time.Since(start).Round(time.Millisecond))
}

// buildChunks 分块 + 分词 + 组装存储行。search_text 与 content 分离：原文供
// LLM 与偏移回链，分词文本仅供 FTS 索引（换分词器无需动原文即可重建）。
func (s *Service) buildChunks(text string) ([]*storage.DocumentChunk, error) {
	a := SharedAnalyzer()
	pieces := ChunkText(text)
	out := make([]*storage.DocumentChunk, 0, len(pieces))
	for i, p := range pieces {
		trimmed := strings.TrimSpace(p.Content)
		if trimmed == "" {
			continue
		}
		sum := sha256.Sum256([]byte(trimmed))
		out = append(out, &storage.DocumentChunk{
			DocumentID:       "",
			ChunkIndex:       i,
			SectionPath:      "", // M1 无可靠章节结构，留空不强行造树
			Content:          p.Content,
			SearchText:       a.SearchText(trimmed),
			StartOffset:      p.StartOffset,
			EndOffset:        p.EndOffset,
			TokenCount:       utf8.RuneCountInString(trimmed),
			ContentHash:      hex.EncodeToString(sum[:]),
			ChunkerVersion:   ChunkerVersion,
			TokenizerVersion: TokenizerVersion,
		})
	}
	return out, nil
}
