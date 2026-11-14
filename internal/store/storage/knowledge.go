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
type DocumentContent struct {
	URL               string    `json:"url"`
	ContentText       string    `json:"contentText"`
	CharCount         int       `json:"charCount"`
	ContentType       string    `json:"contentType"`
	ByteSize          int64     `json:"byteSize"`
	ContentHash       string    `json:"contentHash"`
	ExtractionStatus  string    `json:"extractionStatus"`
	ExtractionVersion int       `json:"extractionVersion"`
	Truncated         bool      `json:"truncated"`
	FetchedAt         time.Time `json:"fetchedAt"`
	UpdatedAt         time.Time `json:"updatedAt"`
}
