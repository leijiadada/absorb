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
	"absorb-agent/internal/store/storage"
	retriever "absorb-agent/tool/pageretriever"
)

// Service 文档准备服务：SSRF 校验 → 提取 → 状态分类 → documents 锚点 +
// document_contents 正文落库。归属维度与问答链路同值（app/user）。
type Service struct {
	app, user string
	st        storage.Storage
	extractor extract.Extractor
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

// Prepare 准备文档。ready/partial 时正文完整落库（partial 表示注入问答时只能
// 覆盖部分正文，全文不受影响）；失败状态不落库（扩展端持临时状态，重试即重新
// 准备）。对外 documentId = 原始 URL：documents 表 (app,user,url) 唯一锚点，
// 第一版原样存 URL 不做规范化，扩展端提问时原样带回。
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
	// partial 语义：进入问答上下文时只能覆盖部分正文（超 PageTextMaxLen 注入截断，
	// 或获取侧已静默截断）；全文仍完整落库，长文档检索是后续增强项。
	status := extract.StatusReady
	if res.Truncated || charCount > retriever.PageTextMaxLen {
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
	// documents 锚点（未来知识来源回链复用）+ 正文缓存
	if _, err := s.st.UpsertDocument(ctx, s.app, s.user, url, title); err != nil {
		return nil, fmt.Errorf("保存文档锚点失败: %w", err)
	}
	if err := s.st.SaveDocumentContent(ctx, s.app, s.user, dc); err != nil {
		return nil, fmt.Errorf("保存文档正文失败: %w", err)
	}
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
