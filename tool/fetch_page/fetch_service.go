package fetch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"trpc.group/trpc-go/trpc-agent-go/tool"
	"trpc.group/trpc-go/trpc-agent-go/tool/webfetch/httpfetch"

	"absorb-agent/internal/log"
)

// defaultMaxContentBytes 单 URL 内容上限默认值（10MB）。
// maxBytesFn 返回 ≤0 时兜底使用；生产配置见 internal/conf FetchConfig.MaxContentMB。
const defaultMaxContentBytes = 10 << 20

// FetchedDocument 公共获取服务的返回：完整正文（无模型侧截断）。
// 供 Agent fetch_page 工具与扩展端文档准备（/api/page/prepare）共用，
// Content 之外的元信息（重定向后地址、状态码）供调用方做登录页误判、
// 错误分类等产品语义判断。
type FetchedDocument struct {
	URL         string // 请求的原始 URL
	FinalURL    string // 重定向后最终地址（httpfetch retrieved_url）
	ContentType string
	StatusCode  int
	ByteSize    int64 // 提取后正文的 UTF-8 字节数
	Content     string
	Truncated   bool // 正文达到内容上限被 httpfetch 截断（PDF 超限直接报错，不走此标记）
}

// FetchError 获取层可分类错误：StatusCode>0 表示服务端非 2xx（401/403 可映射
// "需要登录"等产品状态）；StatusCode=0 表示网络失败、解析失败、内容为空等。
type FetchError struct {
	StatusCode int
	Message    string
}

func (e *FetchError) Error() string { return e.Message }

// FetchPageService 公共 HTTP 获取服务：复用 httpfetch 的正文提取（HTML 主内容
// 聚焦、PDF 文本层解析）与代理解析能力，不做面向模型的截断/语义包装。
// 并发安全；内容上限经 maxBytesFn 每次请求动态读取，变更后下次请求生效
// （与代理地址、超时的热生效约定一致）。
type FetchPageService struct {
	proxyFn    func() string
	maxBytesFn func() int64

	mu       sync.Mutex
	inner    tool.CallableTool // httpfetch 底层工具（内容上限变更时重建）
	innerMax int64
}

// NewFetchPageService 构建公共获取服务。maxBytesFn 为 nil 时使用默认 10MB 上限。
func NewFetchPageService(proxyFn func() string, maxBytesFn func() int64) *FetchPageService {
	if maxBytesFn == nil {
		maxBytesFn = func() int64 { return defaultMaxContentBytes }
	}
	return &FetchPageService{proxyFn: proxyFn, maxBytesFn: maxBytesFn}
}

// httpTool 返回底层 httpfetch 工具及其生效的内容上限；上限变化时按新值重建
// （构造期选项，无法原地更新）。代理地址经 ProxyResolver 每次请求动态解析，
// 重建只为上限生效。
func (s *FetchPageService) httpTool() (inner tool.CallableTool, maxBytes int64, err error) {
	maxBytes = s.maxBytesFn()
	if maxBytes <= 0 {
		maxBytes = defaultMaxContentBytes
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inner == nil || s.innerMax != maxBytes {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.Proxy = ProxyResolver(s.proxyFn)
		inner := httpfetch.NewTool(
			httpfetch.WithMainContentExtraction(true),
			httpfetch.WithMaxContentLength(int(maxBytes)),
			httpfetch.WithHTTPClient(&http.Client{Transport: tr}),
		)
		s.inner, s.innerMax = inner, maxBytes
	}
	return s.inner, maxBytes, nil
}

// ProxyResolver 把代理 getter 转成 transport 的 Proxy 函数：每次请求实时解析当前值。
// 优先级：手动配置 > Windows 系统代理（含例外地址直连）> 环境变量；
// 非空时经该 HTTP 代理出网（国内直连 GitHub 等站点会被阻断），地址非法时告警并本次直连。
// 导出以供模型 API 客户端复用同一套解析逻辑，保证全局代理一致。
func ProxyResolver(proxyFn func() string) func(*http.Request) (*url.URL, error) {
	return func(req *http.Request) (*url.URL, error) {
		p := strings.TrimSpace(proxyFn())
		if p == "" {
			if u, decided := systemProxyLookup(req.URL); decided {
				return u, nil
			}
			return http.ProxyFromEnvironment(req)
		}
		u, err := url.Parse(p)
		if err != nil {
			log.Warnf("[Absorb] fetch_page invalid proxy address (%s), connecting directly: %v", p, err)
			return nil, nil
		}
		return u, nil
	}
}

// Fetch 获取指定 URL 并返回提取后的完整正文。
// 错误统一为 *FetchError，调用方按 StatusCode 分类处理（提示词降级 / 产品状态映射）。
func (s *FetchPageService) Fetch(ctx context.Context, url string) (*FetchedDocument, error) {
	url = strings.TrimSpace(url)
	if url == "" {
		return nil, &FetchError{Message: "URL 不能为空"}
	}

	inner, maxBytes, err := s.httpTool()
	if err != nil {
		return nil, &FetchError{Message: "构建获取客户端失败: " + err.Error()}
	}

	args, err := json.Marshal(map[string][]string{"urls": {url}})
	if err != nil {
		return nil, &FetchError{Message: "构造请求失败: " + err.Error()}
	}

	raw, err := inner.Call(ctx, args)
	if err != nil {
		return nil, &FetchError{Message: "请求失败: " + err.Error()}
	}

	// httpfetch 返回类型化结构体，序列化后解析通用结构
	bts, err := json.Marshal(raw)
	if err != nil {
		return nil, &FetchError{Message: "解析返回结果失败: " + err.Error()}
	}
	var data struct {
		Results []struct {
			RetrievedURL string `json:"retrieved_url"`
			StatusCode   int    `json:"status_code"`
			ContentType  string `json:"content_type"`
			Content      string `json:"content"`
			Error        string `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal(bts, &data); err != nil {
		return nil, &FetchError{Message: "解析返回结果失败: " + err.Error()}
	}
	if len(data.Results) == 0 {
		return nil, &FetchError{Message: "无返回结果"}
	}
	first := data.Results[0]

	if first.StatusCode != 0 && first.StatusCode != 200 {
		msg := first.Error
		if msg == "" {
			msg = fmt.Sprintf("服务端返回状态码: %d", first.StatusCode)
		}
		return nil, &FetchError{StatusCode: first.StatusCode, Message: msg}
	}
	if first.Error != "" {
		return nil, &FetchError{StatusCode: first.StatusCode, Message: first.Error}
	}
	if strings.TrimSpace(first.Content) == "" {
		return nil, &FetchError{Message: "页面未包含可读文本内容"}
	}

	return &FetchedDocument{
		URL:         url,
		FinalURL:    first.RetrievedURL,
		ContentType: first.ContentType,
		StatusCode:  first.StatusCode,
		ByteSize:    int64(len(first.Content)),
		Content:     first.Content,
		Truncated:   maxBytes > 0 && int64(len(first.Content)) >= maxBytes,
	}, nil
}
