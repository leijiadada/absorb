// 索引构建（M1）：document_contents 全文 → 分块 → 分词 → 事务内整体替换写入
// document_chunks / document_chunks_fts / document_index_state。
// 索引是派生数据：构建失败不阻塞 prepare（提取是事实、索引是加速器），
// 只标 failed 待重建；重建由下次 prepare 或未来按 index_state 调度。
package document

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"absorb-agent/internal/document/extract"
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
// 幂等判断按词法/向量两个域拆分（0010 起）：
//   - 词法域：index_state ready 且 content_hash / 切块与分词版本均未变化时跳过
//     （重复 prepare 不重复分词——20 万字正文省 1~2s，也是 document_index_state
//     存在的本意：事实数据与派生索引的生命周期分离）。
//   - 向量域：vector_status=ready 且 vector_model=当前模型 且 vector_count=chunk_count
//     才视为就绪——向量缺失、上次 embed 失败、换模型都能被独立检测并补建，
//     不再被词法 ready 挡住（P0 修复）。补建复用库内现有 chunks，不重写词法索引。
func (s *Service) BuildIndex(ctx context.Context, documentID string, dc *storage.DocumentContent) {
	if documentID == "" || dc == nil || strings.TrimSpace(dc.ContentText) == "" {
		return
	}
	if st, err := s.st.LoadDocumentIndexState(ctx, s.app, s.user, documentID); err == nil && st != nil &&
		st.IndexStatus == storage.IndexStatusReady &&
		st.IndexedContentHash == dc.ContentHash &&
		st.ChunkerVersion == ChunkerVersion &&
		st.TokenizerVersion == TokenizerVersion {
		// 词法已就绪：是否需要补向量只由向量域决定
		if s.embedder == nil {
			return
		}
		if st.VectorStatus == storage.VectorStatusReady &&
			st.VectorModel == s.embedder.Model() &&
			st.VectorCount == st.ChunkCount {
			return
		}
		log.Infof("[Absorb] document index lexical ready, vectors stale/missing (doc=%s, vector_status=%q, model=%q) — re-embed only",
			documentID, st.VectorStatus, st.VectorModel)
		s.embedChunks(ctx, documentID, nil)
		return
	}
	start := time.Now()
	state := &storage.DocumentIndexState{
		DocumentID:         documentID,
		IndexedContentHash: dc.ContentHash,
		ChunkerVersion:     ChunkerVersion,
		TokenizerVersion:   TokenizerVersion,
	}
	chunks, err := s.buildChunks(dc.ContentText, parseHeadingMarks(dc.HeadingMarks))
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

// embedChunks 索引期语义向量化（M3）：chunks 已就绪后按 chunk_index 批量向量
// 入库。chunks 为 nil 时复用库内现有 chunks（词法 ready、向量缺失/失败/换模型的
// 补建路径——不重写词法索引）。向量是派生数据的派生数据：失败标 failed 只告警，
// 下次 prepare/BuildIndex 自动补建；chunk_id 与库内 chunks 一致，余弦路按 model
// 过滤加载。向量域状态经 UpdateDocumentVectorState 独立记录（不与词法 ready 混用）。
func (s *Service) embedChunks(ctx context.Context, documentID string, chunks []*storage.DocumentChunk) {
	if s.embedder == nil {
		return
	}
	if chunks == nil {
		var err error
		chunks, err = s.st.LoadDocumentChunks(ctx, s.app, s.user, documentID)
		if err != nil {
			log.Warnf("[Absorb] chunk vectors: load chunks failed (doc=%s): %v", documentID, err)
			return
		}
		if len(chunks) == 0 {
			log.Warnf("[Absorb] chunk vectors: no chunks to embed (doc=%s)", documentID)
			return
		}
	}
	start := time.Now()
	model := s.embedder.Model()
	texts := make([]string, len(chunks))
	rows := make([]*storage.ChunkVectorRow, len(chunks))
	for i, c := range chunks {
		texts[i] = embeddingInput(c)
		rows[i] = &storage.ChunkVectorRow{ChunkIndex: c.ChunkIndex}
	}
	vecs, err := s.embedder.EmbedDocuments(ctx, texts)
	if err != nil {
		log.Warnf("[Absorb] chunk embedding failed (doc=%s, lexical-only until next prepare): %v", documentID, err)
		s.markVectorState(ctx, documentID, storage.DocumentVectorState{
			Model: model, Status: storage.VectorStatusFailed, Error: err.Error(),
		})
		return
	}
	for i, v := range vecs {
		rows[i].ChunkID = chunks[i].ID
		rows[i].DocumentID = documentID
		rows[i].Dims = len(v)
		rows[i].Model = model
		rows[i].Vector = v
	}
	if err := s.st.ReplaceDocumentVectors(ctx, s.app, s.user, documentID, model, rows); err != nil {
		log.Warnf("[Absorb] chunk vector persist failed (doc=%s): %v", documentID, err)
		s.markVectorState(ctx, documentID, storage.DocumentVectorState{
			Model: model, Status: storage.VectorStatusFailed, Error: err.Error(),
		})
		return
	}
	s.markVectorState(ctx, documentID, storage.DocumentVectorState{
		Model: model, Status: storage.VectorStatusReady, Count: len(rows),
	})
	log.Infof("[Absorb] chunk vectors built | doc=%s count=%d model=%s dur=%s",
		documentID, len(rows), model, time.Since(start).Round(time.Millisecond))
}

// markVectorState 向量域状态落库（失败仅告警——状态记录本身不中断主流程）。
func (s *Service) markVectorState(ctx context.Context, documentID string, vs storage.DocumentVectorState) {
	if err := s.st.UpdateDocumentVectorState(ctx, s.app, s.user, documentID, vs); err != nil {
		log.Warnf("[Absorb] vector state update failed (doc=%s): %v", documentID, err)
	}
}

// embeddingInput 索引期向量输入（F3c，2026-11-18）：纯正文。F3a 归因实证：
// 面包屑前缀在 Qwen3-Embedding-4B 上稀释正文语义（48 题两轮稳定 -7pp
// Recall@5，词法路逐题不变、伤害全走向量路）——向量吃纯正文，结构只走
// FTS section_path 加权元数据层。v4 时代 HTML 面包屑前置（借鉴 WeKnora
// ContextHeader）随同规则移除：正文内 # 标题已携带结构、前置冗余且从无
// A/B 证据；统一规则使 re-embed 路径无需文档类型即可复现一致输入。
func embeddingInput(c *storage.DocumentChunk) string {
	return c.Content
}

// buildChunks 分块 + 分词 + 组装存储行。search_text 与 content 分离：原文供
// LLM 与偏移回链，分词文本仅供 FTS 索引（换分词器无需动原文即可重建）。
// SectionPath 为标题面包屑：HTML 路径来自 chunker v4（# 标题行分区）；PDF
// 路径来自 marks 中点规则赋值（F3a/F3c 定稿形态——块边界与 plain 基线逐位
// 一致，结构只进元数据层；F3b 软边界切块两轮评测 -5pp 未达标已回滚，
// 见 design/5.md）。marks 为空时不赋值（HTML/旧文档不变）。
func (s *Service) buildChunks(text string, marks []extract.HeadingMark) ([]*storage.DocumentChunk, error) {
	a := SharedAnalyzer()
	pieces := ChunkText(text)
	assignMarkSectionPaths(pieces, marks)
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
			SectionPath:      p.SectionPath,
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

// parseHeadingMarks 解析落库的 heading_marks JSON（F3a）。空串（HTML 路径/
// 旧文档）返回 nil，全链路行为不变；解析失败按无结构处理并告警（marks 是
// 派生元数据，宁缺勿错，不阻塞索引构建）。Offset 升序排序保证赋值二分正确。
func parseHeadingMarks(s string) []extract.HeadingMark {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var marks []extract.HeadingMark
	if err := json.Unmarshal([]byte(s), &marks); err != nil {
		log.Warnf("[Absorb] heading_marks parse failed (ignored): %v", err)
		return nil
	}
	sort.SliceStable(marks, func(i, j int) bool { return marks[i].Offset < marks[j].Offset })
	return marks
}

// assignMarkSectionPaths F3a/F3c 定稿形态（2026-11-18 评审契约）：chunk 的
// SectionPath = 满足 mark.Offset ≤ chunk.Start + chunkLen/2 的最后一个 mark
// 的面包屑——标题命名其后内容，取中点让「标题后内容占块多数」的块归新章节。
// 只补元数据不动块边界：Content/StartOffset/EndOffset 均为 ChunkText 原样
// 产物，与 plain 基线逐位一致。marks 为空不赋值（HTML 路径走 chunker v4）；
// 中点在首个标题前（导语区）保持空面包屑。（F3b 软边界切块曾替代本函数，
// 两轮 -5pp 未达标回滚——F3b 实验记录见 design/5.md。）
func assignMarkSectionPaths(pieces []textChunk, marks []extract.HeadingMark) {
	if len(pieces) == 0 || len(marks) == 0 {
		return
	}
	for i := range pieces {
		c := &pieces[i]
		mid := c.StartOffset + (c.EndOffset-c.StartOffset)/2
		j := sort.Search(len(marks), func(k int) bool { return marks[k].Offset > mid }) - 1
		if j < 0 {
			continue
		}
		c.SectionPath = markBreadcrumb(marks[j])
	}
}

// markBreadcrumb mark 面包屑拼接：Path 链（含自身标题）逐级截断（v4 同款
// 单级 40 rune）后以 " > " 连接，总长截断（v4 同款 96 rune 自最老一级丢弃，
// 深层标题更具区分度）——与 HTML 路径 breadcrumbPaths 产物同形态。
func markBreadcrumb(m extract.HeadingMark) string {
	parts := make([]string, 0, len(m.Path))
	for _, t := range m.Path {
		parts = append(parts, truncateRunes(t, headingTitleMax))
	}
	return truncatePath(strings.Join(parts, " > "))
}
