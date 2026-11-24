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
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"absorb-agent/internal/log"
	fetch_page "absorb-agent/tool/fetch_page"
)

// 产品状态枚举（prepare 链路对外语义，document_contents.extraction_status 同值域）。
// 失败状态不落库，由扩展端渲染对应文案并决定重试。
const (
	StatusReady        = "ready"         // 完整提取（全文已落库；注入是否截断属上下文交付，不影响本状态）
	StatusPartial      = "partial"       // 获取侧静默截断（提取不完整）；注入长度限制属上下文交付，不改变本状态
	StatusTooLarge     = "too_large"     // 文件超内容大小上限
	StatusNeedsAuth    = "needs_auth"    // 401/403：需要登录或访问权限
	StatusNoText       = "no_text"       // 无可提取文本（扫描件/图片型 PDF）
	StatusEncrypted    = "encrypted"     // 加密/密码保护
	StatusInvalidURL   = "invalid_url"   // URL 非法或指向内网（SSRF 拦截）
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

// Result 提取产物：完整正文（无模型侧截断）与获取元信息。Marks 为 PDF 章节
// 标题元数据（F3a，仅 PDF 路径可能非空）——正文一个字节不动，结构只进元
// 数据层；必须随正文落库（BuildIndex 只有文本无 PDF 字节，docling 无法重算）。
type Result struct {
	Content     string
	ContentType string
	FinalURL    string        // 重定向后最终地址（登录页误判排查用）
	Truncated   bool          // 获取侧静默截断（HTML/text 达上限；PDF 超限直接报错不走此标记）
	Marks       []HeadingMark // 章节标题元数据（rune 偏移+层级+标题+面包屑链），见 structure.go
	Diagnostics string        // 提取决策摘要（择优切换/TOC 删除量/marks 数/引擎失败原因），供 prepare 日志打印
}

// Extractor 文档正文提取器（格式接缝）：获取 URL 并提取完整正文。
// 失败返回 *Error（Status 已分类），供 Service 透传为产品状态。
type Extractor interface {
	Extract(ctx context.Context, url string) (*Result, error)
}

// PDFExtractor 基于 FetchPageService 的提取器：httpfetch 负责 PDF 文本层解析
// 与 HTML 主内容聚焦，本层不做面向模型的截断/包装。proxyFn/maxBytesFn 供
// poppler 第二引擎按同一代理链与内容上限下载原始字节（见 poppler.go 择优）。
type PDFExtractor struct {
	Fetch      *fetch_page.FetchPageService
	proxyFn    func() string
	maxBytesFn func() int64
}

// NewPDFExtractor 构建提取器。fetch 为 nil 时 Extract 返回网络错误；
// proxyFn/maxBytesFn 为 poppler 双跑择优的字节下载参数，可为 nil（禁用择优）。
func NewPDFExtractor(f *fetch_page.FetchPageService, proxyFn func() string, maxBytesFn func() int64) *PDFExtractor {
	return &PDFExtractor{Fetch: f, proxyFn: proxyFn, maxBytesFn: maxBytesFn}
}

// Extract 获取并提取正文；获取层错误经 classify 映射为产品状态。
// PDF 内容类型走"双跑择优"：主路（httpfetch/dslipak 文本层）之外并行调用
// pdftotext（poppler），质量分显著更低才切换（见 poppler.go）；pdftotext
// 缺失或失败时主路结果原样返回，行为与历史版本一致。
func (p *PDFExtractor) Extract(ctx context.Context, url string) (*Result, error) {
	if p.Fetch == nil {
		return nil, &Error{Status: StatusNetworkError, Message: "获取服务未初始化"}
	}
	doc, err := p.Fetch.Fetch(ctx, url)
	if err != nil {
		return nil, classify(err)
	}
	content := CollapsePDFSpacing(doc.Content)
	// 乱码守卫：字体缺 ToUnicode cmap 时（实测《人月神话》某文字版、《程序员
	// 的自我修养》），dslipak 提取出替换符/空串——落库只会污染索引，按扫描件
	// 同语义拒绝（no_text：无可提取文本）。当前Bad 时由 poppler 第二引擎接管。
	currentBad := garbledRatio(content) > 0.05

	best, data, switched := p.betterPopplerText(ctx, url, doc.ContentType, content, currentBad)
	if switched {
		content = best
		currentBad = false
	}
	// 页眉/页脚过滤：剔除分页文本中逐页重复的页首/页尾行。必须在 docling
	// 结构叠加之前，HeadingMark 偏移以过滤后文本为基准；\f 数量与位置不变。
	// 主路连续文本（无 \f）跳过。
	var furnitureLines int
	if strings.Contains(content, "\f") {
		content, furnitureLines = stripPDFFurniture(content)
	}
	// 结构进元数据（F3a 形态，门控已放开）：docling 章节树不再注入正文——匹配
	// 产出 HeadingMark 元数据（偏移+层级+面包屑），切块后按中点规则赋 SectionPath。
	// 门控（仅主路保留文本才生成 marks）已撤：正文一字节不动、向量吃纯正文（F3c），
	// P1 干净文本上叠加的 -7pp 来自标题行注入扰动块边界，该路径已不存在；F7 实测
	// poppler 文本上 MarksForDiag 命中 192、覆盖 99.4%。仅在无 PDF 原始字节
	// （poppler 未运行/下载失败，docling 无法解析章节树）时跳过。
	var marks []HeadingMark
	marksHit := 0
	if data != nil {
		secs, docPages := doclingSections(data)
		marks, marksHit = overlayMarks(content, secs, docPages)
		// F8 hybrid：用 PDF 原生书签树的完整标题/层级修正 docling Path（断行、
		// 错配），并在 docling 质量门挡掉的书（人月神话等 marks=0）上直接由书签
		// 生成 marks。只改 Path / 新建 mark，正文不变；失败静默回退 docling。
		// 必须在 stripTOC 之前——后者会改 Offset，页窗口匹配依赖原始分页文本。
		var outlineHit int
		marks, outlineHit = HybridOutlinePaths(ctx, marks, data, content)
		if outlineHit > 0 {
			log.Infof("[Absorb] outline hybrid | url=%s covered=%d total_marks=%d", url, outlineHit, len(marks))
			if len(marks) > marksHit {
				marksHit = len(marks)
			}
		}
	}
	if currentBad {
		return nil, &Error{Status: StatusNoText, Message: "文本无法解码（PDF 字体缺少 Unicode 映射，可能是扫描件或特殊编码）"}
	}
	// 目录页剥离（P5）：页级整删（poppler 分页文本，密度判定）+ 行级条目行，
	// 放在结构匹配之后——页级删除会移除分页符使页号前移，匹配的 ±2 页窗口
	// 依赖原始分页；剥离 mark 感知（F3a）：删目录页/行时丢弃对应 marks、
	// 其余偏移前移修正，维持「text[offset:] 以 Title 开头」不变式
	var tocPages, tocLines int
	content, marks, tocPages, tocLines = stripTOCWithStats(content, marks)
	diagnostics := fmt.Sprintf("marks=%d tocPages=%d tocLines=%d furniture=%d popplerSwitched=%t",
		marksHit, tocPages, tocLines, furnitureLines, switched)
	// PDF 文本层的逐字符空格化修复（CID 字体常见现象，实测《Go 程序设计
	// 语言》中文版整本 "最 早 参 与"——不分词不可用、原文呈现不可读）
	return &Result{
		Content:     content,
		ContentType: doc.ContentType,
		FinalURL:    doc.FinalURL,
		Truncated:   doc.Truncated,
		Marks:       marks,
		Diagnostics: diagnostics,
	}, nil
}

// betterPopplerText 跑 pdftotext 第二引擎并与主路文本择优。仅在确认 PDF
// 内容类型时执行——HTML 主路无需第二引擎，避免每次页面准备的二次下载。
// 引擎缺失/下载/提取任一失败都静默保持主路结果（择优是增强不是依赖）。
// 返回择优文本、poppler 路使用的原始字节（供结构叠加复用，未跑 poppler 时
// 为 nil）与是否发生了切换。
func (p *PDFExtractor) betterPopplerText(ctx context.Context, url, contentType, current string, currentBad bool) (string, []byte, bool) {
	bin := pdftotextBin()
	if bin == "" || !strings.Contains(strings.ToLower(contentType), "pdf") {
		return "", nil, false
	}
	data, err := p.fetchBytes(ctx, url)
	if err != nil || len(data) == 0 {
		return "", nil, false
	}
	alt, err := PopplerText(ctx, bin, data)
	if err != nil {
		if ctx.Err() == nil { // 请求取消是预期行为，不刷屏
			log.Warnf("[Absorb] poppler extract failed (url=%s): %v", url, err)
		}
		return "", data, false
	}
	best, switched := pickBetterText(current, currentBad, alt)
	return best, data, switched
}

// fetchBytes 下载原始 PDF 字节：与主路共用代理解析链与内容上限（超限按
// 失败处理保持主路结果，与 httpfetch 的 PDF 超限报错语义对齐）。
func (p *PDFExtractor) fetchBytes(ctx context.Context, url string) ([]byte, error) {
	maxBytes := int64(0)
	if p.maxBytesFn != nil {
		maxBytes = p.maxBytesFn()
	}
	if maxBytes <= 0 {
		maxBytes = 10 << 20
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = fetch_page.ProxyResolver(p.proxyFn)
	client := &http.Client{Transport: tr, Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("poppler: 下载失败 status=%d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, errors.New("poppler: 原始字节超内容上限")
	}
	return data, nil
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
