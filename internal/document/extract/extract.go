// Package extract 文档正文提取（PDF 解读 PR2）：把"获取 + 提取"封装为产品级
// 文档准备能力，供 /api/page/prepare 使用——不依赖模型按需调用 fetch_page。
//
// Extractor 是格式接缝：PDF 先行（复用 FetchPageService/httpfetch 文本层）；
// 未来 docx/xlsx 等本质是 zip+XML 容器，届时新增实现自取原始字节解包即可接入，
// document.Service 与存储/上下文链路无需改动。
package extract

import (
	"context"
	"errors"
	"strings"

	fetch_page "absorb-agent/tool/fetch_page"
)

// 产品状态枚举（prepare 链路对外语义，document_contents.extraction_status 同值域）。
// 失败状态不落库，由扩展端渲染对应文案并决定重试。
const (
	StatusReady        = "ready"        // 完整提取，全文可注入
	StatusPartial      = "partial"      // 部分可读：全文已落库，但超 PageTextMaxLen 注入截断或获取侧截断
	StatusTooLarge     = "too_large"    // 文件超内容大小上限
	StatusNeedsAuth    = "needs_auth"   // 401/403：需要登录或访问权限
	StatusNoText       = "no_text"      // 无可提取文本（扫描件/图片型 PDF）
	StatusEncrypted    = "encrypted"    // 加密/密码保护
	StatusInvalidURL   = "invalid_url"  // URL 非法或指向内网（SSRF 拦截）
	StatusNetworkError = "network_error" // 网络/服务端/解析失败
)

// Injectable 状态是否可注入问答上下文（只有拿到了正文的两种状态）。
func Injectable(status string) bool {
	return status == StatusReady || status == StatusPartial
}

// Error 提取层可分类错误：Status 为 Status* 产品状态，调用方按状态渲染文案。
type Error struct {
	Status  string
	Message string
}

func (e *Error) Error() string { return e.Message }

// Result 提取产物：完整正文（无模型侧截断）与获取元信息。
type Result struct {
	Content     string
	ContentType string
	FinalURL    string // 重定向后最终地址（登录页误判排查用）
	Truncated   bool   // 获取侧静默截断（HTML/text 达上限；PDF 超限直接报错不走此标记）
}

// Extractor 文档正文提取器（格式接缝）：获取 URL 并提取完整正文。
// 失败返回 *Error（Status 已分类），供 Service 透传为产品状态。
type Extractor interface {
	Extract(ctx context.Context, url string) (*Result, error)
}

// PDFExtractor 基于 FetchPageService 的提取器：httpfetch 负责 PDF 文本层解析
// 与 HTML 主内容聚焦，本层不做面向模型的截断/包装。
type PDFExtractor struct {
	Fetch *fetch_page.FetchPageService
}

// NewPDFExtractor 构建提取器。fetch 为 nil 时 Extract 返回网络错误。
func NewPDFExtractor(f *fetch_page.FetchPageService) *PDFExtractor {
	return &PDFExtractor{Fetch: f}
}

// Extract 获取并提取正文；获取层错误经 classify 映射为产品状态。
func (p *PDFExtractor) Extract(ctx context.Context, url string) (*Result, error) {
	if p.Fetch == nil {
		return nil, &Error{Status: StatusNetworkError, Message: "获取服务未初始化"}
	}
	doc, err := p.Fetch.Fetch(ctx, url)
	if err != nil {
		return nil, classify(err)
	}
	return &Result{
		Content:     doc.Content,
		ContentType: doc.ContentType,
		FinalURL:    doc.FinalURL,
		Truncated:   doc.Truncated,
	}, nil
}

// classify 把 FetchError 映射为产品状态（httpfetch 错误语义，见其 fetch.go：
// PDF 超限 "pdf response body exceeds per-url content limit"；加密走
// "read pdf: ..." 解析错误；扫描件提取出空文本）。
func classify(err error) error {
	var fe *fetch_page.FetchError
	if !errors.As(err, &fe) {
		return &Error{Status: StatusNetworkError, Message: err.Error()}
	}
	msg := fe.Message
	lower := strings.ToLower(msg)
	switch {
	case fe.StatusCode == 401 || fe.StatusCode == 403:
		return &Error{Status: StatusNeedsAuth, Message: "文档需要登录或访问权限（桌面端不会继承浏览器 Cookie）: " + msg}
	case strings.Contains(lower, "exceeds per-url content limit"):
		return &Error{Status: StatusTooLarge, Message: "文档超过当前内容大小上限，可在设置中调大 fetch 内容上限后重试"}
	case strings.Contains(lower, "encrypt") || strings.Contains(lower, "password"):
		return &Error{Status: StatusEncrypted, Message: "文档受密码保护或已加密，暂不支持提取"}
	case strings.Contains(msg, "未包含可读文本"):
		return &Error{Status: StatusNoText, Message: "未提取到文本内容（可能是扫描件或图片型 PDF）"}
	default:
		return &Error{Status: StatusNetworkError, Message: msg}
	}
}
