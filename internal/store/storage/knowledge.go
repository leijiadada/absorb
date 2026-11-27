package storage

import "time"

// 知识条目生命周期状态（knowledge_items.status）。
const (
	KnowledgeStatusActive   = "active"   // 在册：参与列表与后续吸收闭环
	KnowledgeStatusArchived = "archived" // 归档：软删除，可重新打开
)

// 来源类型（knowledge_sources.source_type）。
const (
	// KnowledgeSourceQuote 划词原文证据：quote_text 必为源材料真实文本。
	KnowledgeSourceQuote = "quote"
	// KnowledgeSourceConversation 对话来源：无划词轮次，quote_text 恒为空串
	// （设计文档 §五：无法可靠定位原文就明确缺少依据，不得塞入提问等非原文文本）。
	KnowledgeSourceConversation = "conversation"
)

// KnowledgeHit 检索候选召回结果（M3.1）：条目 + 其全部来源。
// 打分与阈值过滤在知识服务层完成，存储层只做关键词 LIKE 预筛。
type KnowledgeHit struct {
	Item    *KnowledgeItem
	Sources []*KnowledgeSource
}

// KnowledgeItem knowledge_items 表行：可变的知识对象。
// AppName/UserID/DedupKey 为内部实现细节（DedupKey 由知识服务层计算，
// 调用方不可传），均不回传前端。
type KnowledgeItem struct {
	ID                string    `json:"id"`
	AppName           string    `json:"-"`
	UserID            string    `json:"-"`
	Title             string    `json:"title"`
	AISummary         string    `json:"aiSummary"`         // AI 提炼，用户可编辑
	UserUnderstanding string    `json:"userUnderstanding"` // 个人理解，可为空（lcb.md 允许）
	Status            string    `json:"status"`
	OriginSessionID   string    `json:"originSessionId"` // 产生该知识的会话（回链指针，非依赖）
	OriginEventID     string    `json:"originEventId"`   // 产生该知识的事件（invocationId）
	DedupKey          string    `json:"-"`               // 幂等锚点：sha256(origin_session_id ‖ origin_event_id)
	CreatedAt         time.Time `json:"createdAt"`
	UpdatedAt         time.Time `json:"updatedAt"`
}

// KnowledgeSource knowledge_sources 表行：不可变的证据（quote_text 永不被
// 编辑路径触碰），一条知识允许多来源（1:N）。lcb.md 中的 knowledge_id
// 概念名对应此处的 ItemID（表列 item_id）。
type KnowledgeSource struct {
	ID               string    `json:"id"`
	ItemID           string    `json:"itemId"`
	AppName          string    `json:"-"`
	UserID           string    `json:"-"`
	SourceType       string    `json:"sourceType"` // quote | conversation
	SourceTitle      string    `json:"sourceTitle"`
	SourceURL        string    `json:"sourceUrl"`
	QuoteText        string    `json:"quoteText"`       // conversation 来源恒为空串
	SourceSessionID  string    `json:"sourceSessionId"` // 事件定位二元组之一（来源可来自后续会话）
	SourceEventID    string    `json:"sourceEventId"`
	SourceDocumentID string    `json:"sourceDocumentId"` // documents 聚合锚点（无 URL 来源为空）
	CreatedAt        time.Time `json:"createdAt"`
}

// Document documents 表行：按 URL 聚合的原始资料锚点（M2.5）。
// summary_* 是阅读摘要第一版落点（AI 对文章的概括，不代表用户观点）；
// created/updated 由存储层填充。
type Document struct {
	ID               string    `json:"id"`
	AppName          string    `json:"-"`
	UserID           string    `json:"-"`
	URL              string    `json:"url"`
	Title            string    `json:"title"`
	SummaryText      string    `json:"summaryText"`
	SummarySessionID string    `json:"summarySessionId"`
	SummaryEventID   string    `json:"summaryEventId"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

// DocumentContent document_contents 表行：文档提取正文缓存（PDF 解读 PR2）。
// 身份与 documents 同锚点（app,user,url）；ContentText 保存完整提取正文
// （不受聊天上下文限制，注入时才截断）。失败状态（too_large/needs_auth 等）
// 不落库，本表只有可复用正文；时间戳由存储层填充。
// HeadingMarks（F3a）：PDF 章节标题元数据 JSON（[]extract.HeadingMark：rune
// 偏移+层级+标题+面包屑链）——正文一个字节不动，结构只进元数据层。存储层
// 持有不透明字符串（不引入 extract 依赖）；空串 = 无结构（HTML 路径/旧文档，
// 全链路行为不变）。Marks 必须落库：BuildIndex 只有文本无 PDF 字节，docling
// 无法重算。
type DocumentContent struct {
	URL               string    `json:"url"`
	ContentText       string    `json:"contentText"`
	CharCount         int       `json:"charCount"`
	ContentType       string    `json:"contentType"`
	ByteSize          int64     `json:"byteSize"` // 提取正文 UTF-8 字节长度（len(ContentText)，非原始文件大小）
	ContentHash       string    `json:"contentHash"`
	ExtractionStatus  string    `json:"extractionStatus"`
	ExtractionVersion int       `json:"extractionVersion"`
	Truncated         bool      `json:"truncated"`
	HeadingMarks      string    `json:"headingMarks,omitempty"` // 章节标题元数据 JSON（F3a），空 = 无结构
	FetchedAt         time.Time `json:"fetchedAt"`
	UpdatedAt         time.Time `json:"updatedAt"`
}

// DocumentChunk document_chunks 表行：检索片段（M1，document_contents 的派生物，
// 可随时整体重建）。Content 原文供 LLM，SearchText 分词文本供 FTS 索引；
// StartOffset/EndOffset 为正文 rune 偏移（回链 content_text 取前后原文）；
// CreatedAt 由存储层填充。
type DocumentChunk struct {
	ID               string    `json:"id"`
	AppName          string    `json:"-"`
	UserID           string    `json:"-"`
	DocumentID       string    `json:"documentId"`
	ChunkIndex       int       `json:"chunkIndex"`
	SectionPath      string    `json:"sectionPath,omitempty"`
	Content          string    `json:"content"`
	SearchText       string    `json:"-"`
	StartOffset      int       `json:"startOffset"`
	EndOffset        int       `json:"endOffset"`
	TokenCount       int       `json:"tokenCount"`
	ContentHash      string    `json:"contentHash"`
	ChunkerVersion   int       `json:"chunkerVersion"`
	TokenizerVersion string    `json:"tokenizerVersion"`
	CreatedAt        time.Time `json:"createdAt"`
}

// 索引状态值（document_index_state.index_status）。
const (
	IndexStatusPending = "pending" // 未建/待重建（含 hash 或版本不匹配）
	IndexStatusReady   = "ready"   // chunks 与 FTS 均已就绪
	IndexStatusFailed  = "failed"  // 构建失败（旧索引保留，ErrorMessage 记原因）
)

// 向量索引状态值（document_index_state.vector_status，0010 起）。
// ” = 未建/未知（含 chunks 刚重建、向量待补）。向量域独立于词法 index_status：
// ready 且 vector_model=当前模型 且 vector_count=chunk_count 才视为向量就绪。
const (
	VectorStatusReady  = "ready"
	VectorStatusFailed = "failed"
)

// DocumentIndexState document_index_state 表行：文档索引生命周期。
// IndexedContentHash 比对 document_contents.content_hash 判定待重建。
// 词法域（IndexStatus/ChunkerVersion/TokenizerVersion）与向量域（Vector*）解耦：
// 词法 ready 不代表向量完整——向量缺失/失败/换模型须能被独立检测并补建。
type DocumentIndexState struct {
	AppName            string    `json:"-"`
	UserID             string    `json:"-"`
	DocumentID         string    `json:"documentId"`
	IndexedContentHash string    `json:"indexedContentHash"`
	IndexStatus        string    `json:"indexStatus"`
	ChunkerVersion     int       `json:"chunkerVersion"`
	TokenizerVersion   string    `json:"tokenizerVersion"`
	ChunkCount         int       `json:"chunkCount"`
	IndexedAt          time.Time `json:"indexedAt"`
	ErrorMessage       string    `json:"errorMessage,omitempty"`
	// 向量索引状态（0010 起）：embedding 模型指纹 + 完成度。
	VectorModel  string `json:"vectorModel"`
	VectorStatus string `json:"vectorStatus"`
	VectorCount  int    `json:"vectorCount"`
	VectorError  string `json:"vectorError,omitempty"`
}

// DocumentVectorState 向量域状态写入载荷（UpdateDocumentVectorState 用）：
// 只覆盖 document_index_state 的 vector_* 四列，不触碰词法状态。
type DocumentVectorState struct {
	Model  string `json:"model"`
	Status string `json:"status"`
	Count  int    `json:"count"`
	Error  string `json:"error,omitempty"`
}

// DocumentChunkHit FTS 检索命中：chunk 原文 + 所属文档标题 + bm25 得分。
// Score 为 bm25() 原始值（负数，越小越相关），不与其他评分体系混用。
type DocumentChunkHit struct {
	DocumentID  string  `json:"documentId"`
	Title       string  `json:"title"`
	ChunkIndex  int     `json:"chunkIndex"`
	SectionPath string  `json:"sectionPath,omitempty"`
	Content     string  `json:"content"`
	StartOffset int     `json:"startOffset"`
	EndOffset   int     `json:"endOffset"`
	Page        int     `json:"page,omitempty"` // 页号（工具层按正文 \f 边界填充；0 = 无分页信息）
	Score       float64 `json:"score"`
}

// ChunkVectorRow document_chunk_vectors 行（M3 语义召回）：chunk_id 关联
// document_chunks；Vector 为 float32 切片（存储层负责 BLOB 编解码）。
// Model 随行存储——跨模型向量不可比，加载按当前模型过滤。
type ChunkVectorRow struct {
	ChunkID    string    `json:"chunkId"`
	DocumentID string    `json:"documentId"`
	ChunkIndex int       `json:"chunkIndex"`
	Dims       int       `json:"dims"`
	Model      string    `json:"model"`
	Vector     []float32 `json:"-"`
	CreatedAt  time.Time `json:"createdAt"`
}
