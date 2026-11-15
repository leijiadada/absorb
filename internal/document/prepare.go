// Package document 文档准备服务（PDF 解读 PR2）：识别 → 获取提取 → 状态分类
// → 落库。与 fetch_page 工具共用底层获取能力（FetchPageService），但独立成
// 产品级流程——文档准备的完成度由明确的状态表达，而非依赖模型按需调用工具。
package document

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"absorb-agent/internal/document/extract"
	"absorb-agent/internal/log"
	"absorb-agent/internal/retrieval"
	"absorb-agent/internal/store/storage"
)

// Service 文档准备服务：SSRF 校验 → 提取 → 状态分类 → documents 锚点 +
// document_contents 正文落库。归属维度与问答链路同值（app/user）。
type Service struct {
	app, user string
	st        storage.Storage
	extractor extract.Extractor
	embedder  retrieval.Embedder // M3：nil = 语义向量禁用（纯词法）
}

// NewService 构建文档准备服务。extractor 为 PDF 提取器（未来格式经同一接缝注入）。
func NewService(st storage.Storage, ex extract.Extractor, app, user string) *Service {
	return &Service{app: app, user: user, st: st, extractor: ex}
}

// extractionVersion 提取器版本：升级提取逻辑（如换 PDF 解析库）后 +1，
// 配合 content_hash 支撑未来的按版本重提取。
const extractionVersion = 1

// PrepareResult 准备结果：状态与元信息。不含正文——正文只落库，扩展端仅持
// documentId 与状态，避免大文本经扩展消息对象/chrome.storage 往返。
type PrepareResult struct {
	DocumentID  string `json:"documentId"`
	URL         string `json:"url"`
	FinalURL    string `json:"finalUrl,omitempty"`
	Title       string `json:"title,omitempty"`
	ContentType string `json:"contentType,omitempty"`
	Status      string `json:"status"`
	CharCount   int    `json:"charCount"`
	ByteSize    int64  `json:"byteSize"`
	Truncated   bool   `json:"truncated"`
	Message     string `json:"message,omitempty"`
}

// Prepare 准备文档。ready/partial 时正文完整落库（partial 仅表示获取侧截断，
// 与注入上下文的长度限制无关——20.9 万字全文照存，检索增强是后续增强项）；
// 失败状态不落库（扩展端持临时状态，重试即重新准备）。对外 documentId =
// documents.id（UUID，M0 统一身份）：chunks/摘要/知识来源回链共用同一稳定键；
// 扩展端旧卡片持 URL 的情况由问答链路按 URL 回退兼容。
func (s *Service) Prepare(ctx context.Context, rawURL, title string) (*PrepareResult, error) {
	url := strings.TrimSpace(rawURL)
	title = strings.TrimSpace(title)
	start := time.Now()
	base := PrepareResult{DocumentID: url, URL: url, Title: title}

	if url == "" {
		base.Status, base.Message = extract.StatusInvalidURL, "URL 不能为空"
		return &base, nil
	}
	if err := ValidatePublicURL(url); err != nil {
		base.Status, base.Message = extract.StatusInvalidURL, err.Error()
		return &base, nil
	}

	res, err := s.extractor.Extract(ctx, url)
	if err != nil {
		var ee *extract.Error
		if errors.As(err, &ee) {
			base.Status, base.Message = ee.Status, ee.Message
		} else {
			base.Status, base.Message = extract.StatusNetworkError, err.Error()
		}
		log.Warnf("[Absorb] document prepare failed | url=%q status=%s dur=%s msg=%q",
			url, base.Status, time.Since(start).Round(time.Millisecond), base.Message)
		return &base, nil
	}

	text := res.Content
	if strings.TrimSpace(text) == "" {
		base.Status, base.Message = extract.StatusNoText, "未提取到文本内容（可能是扫描件或图片型 PDF）"
		log.Warnf("[Absorb] document prepare empty text | url=%q dur=%s", url, time.Since(start).Round(time.Millisecond))
		return &base, nil
	}

	charCount := len([]rune(text))
	// partial 语义（M0 修正）：仅表达「获取侧已静默截断」（提取完整度）。
	// 超过 PageTextMaxLen 属上下文交付限制（注入时截断、缓存只存开头），
	// 不再混入提取状态——全文完整落库的文档不应被判为 partial。
	status := extract.StatusReady
	if res.Truncated {
		status = extract.StatusPartial
	}
	sum := sha256.Sum256([]byte(text))
	dc := &storage.DocumentContent{
		URL:               url,
		ContentText:       text,
		CharCount:         charCount,
		ContentType:       res.ContentType,
		ByteSize:          int64(len(text)),
		ContentHash:       hex.EncodeToString(sum[:]),
		ExtractionStatus:  status,
		ExtractionVersion: extractionVersion,
		Truncated:         res.Truncated,
		FetchedAt:         time.Now(),
	}
	// documents 锚点（知识来源回链 / 未来 chunks 关联复用）+ 正文缓存；
	// documentId 取锚点 UUID，替代早期的「URL 即 documentId」语义。
	doc, err := s.st.UpsertDocument(ctx, s.app, s.user, url, title)
	if err != nil {
		return nil, fmt.Errorf("保存文档锚点失败: %w", err)
	}
	base.DocumentID = doc.ID
	if err := s.st.SaveDocumentContent(ctx, s.app, s.user, dc); err != nil {
		return nil, fmt.Errorf("保存文档正文失败: %w", err)
	}
	// M1：构建检索索引（分块+分词+FTS）。索引是派生数据，失败只标 failed
	// 不影响本次准备结果；重建由下次读取或 index_state 调度触发。
	s.BuildIndex(ctx, base.DocumentID, dc)
	log.Infof("[Absorb] document prepared | url=%q status=%s type=%q chars=%d bytes=%d dur=%s",
		url, status, res.ContentType, charCount, dc.ByteSize, time.Since(start).Round(time.Millisecond))

	base.FinalURL = res.FinalURL
	base.ContentType = res.ContentType
	base.Status = status
	base.CharCount = charCount
	base.ByteSize = dc.ByteSize
	base.Truncated = res.Truncated
	return &base, nil
}
