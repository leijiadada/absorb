package fetch

import (
	"context"
	"errors"
	"strconv"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/tool"
	"trpc.group/trpc-go/trpc-agent-go/tool/function"
)

// fetch_page 工具：FetchPageService 的 Agent 适配层。
// LLM 需要访问 URL 资源时调用，直接发起 HTTP 请求获取页面内容并转为 Markdown。
// 支持 HTML、纯文本、JSON、PDF。获取与提取能力在 FetchPageService（与扩展端
// 文档准备共用）；本层只负责模型侧约定：20k 字符截断与模型可读的错误消息。

type fetchPageInput struct {
	URL string `json:"url" jsonschema:"description=要获取内容的页面 URL"`
}

type fetchPageOutput struct {
	Success     bool   `json:"success"`
	Content     string `json:"content,omitempty"`
	Message     string `json:"message,omitempty"`
	ContentType string `json:"content_type,omitempty"`
}

// fetchMaxRunes 返回给模型的正文长度上限（按 Unicode 字符计）。
// fetch_page 结果作为工具消息整体进入上下文，极端长文会挤占窗口、稀释召回；
// 超长部分截断并标注（输入窗口 128K 下的保守保护）。
// FetchPageService 返回的是完整正文，截断仅发生在本适配层。
const fetchMaxRunes = 20000

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "\n\n（内容过长已截断，仅保留前 " + strconv.Itoa(n) + " 字符）"
}

// NewFetchPageTool 注册 fetch_page 工具。代理地址、单请求超时、内容上限均由
// fn 在每次调用时实时读取（设置保存即时生效，无需重启）。
func NewFetchPageTool(proxyFn func() string, timeoutFn func() time.Duration, maxBytesFn func() int64) (tool.CallableTool, error) {
	svc := NewFetchPageService(proxyFn, maxBytesFn)
	fn := function.NewFunctionTool(
		func(ctx context.Context, in fetchPageInput) (fetchPageOutput, error) {
			// 单次调用动态超时：每次执行读取当前配置，≤0 = 不限
			if timeoutFn != nil {
				if d := timeoutFn(); d > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, d)
					defer cancel()
				}
			}
			return fetchViaService(ctx, svc, in.URL), nil
		},
		function.WithName("fetch_page"),
		function.WithDescription(
			"获取指定 URL 的页面内容并转为 Markdown 文本（基于 trpc-agent-go httpfetch）。"+
				"支持 HTML、纯文本、JSON、PDF。适用场景：需要访问 URL 资源；需要当前信息但自己没有；或信息自知识截止已变化（如话题更新、需要实时数据）。不需要时不要调用。",
		),
	)
	return fn, nil
}

// fetchViaService 调用公共获取服务并把结果/错误适配为模型可读输出。
// 错误消息文案与既有行为保持一致（FetchError.Message 沿用原话术）。
func fetchViaService(ctx context.Context, svc *FetchPageService, url string) fetchPageOutput {
	doc, err := svc.Fetch(ctx, url)
	if err != nil {
		var fe *FetchError
		if errors.As(err, &fe) {
			return fetchPageOutput{Success: false, Message: fe.Message}
		}
		return fetchPageOutput{Success: false, Message: "请求失败: " + err.Error()}
	}
	return fetchPageOutput{
		Success:     true,
		Content:     truncateRunes(doc.Content, fetchMaxRunes),
		ContentType: doc.ContentType,
	}
}
