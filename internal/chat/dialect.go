package chat

// 服务商方言差异补丁：按变体聚合登记。
// 背景：各服务商对 OpenAI 兼容协议的字段支持参差，SDK 的通用映射会踩坑。
// 新增方言：写一个具名补丁函数（附原因与实测证据注释），在 variantOptions 加一行 case。

import (
	"context"

	"absorb-agent/internal/kind"
	openaigo "github.com/openai/openai-go"
	"trpc.group/trpc-go/trpc-agent-go/model/openai"
)

// variantOptions 返回指定变体需要追加的方言补丁选项。
func variantOptions(v kind.Variant) []openai.Option {
	switch v {
	case kind.VariantGLM:
		return []openai.Option{maxTokensAsLegacyField()}
	default:
		return nil
	}
}

// maxTokensAsLegacyField 把 max_completion_tokens 的值改写为 max_tokens：
// bigmodel（GLM 方言）不认 OpenAI 新版 max_completion_tokens 字段，静默忽略后
// 输出不限长，用户配置的输出上限失效。SDK 仅对 DeepSeek 变体映射 max_tokens，
// 故发送前统一改写回旧字段。
func maxTokensAsLegacyField() openai.Option {
	return openai.WithChatRequestCallback(func(_ context.Context, p *openaigo.ChatCompletionNewParams) {
		if p.MaxCompletionTokens.Valid() {
			p.MaxTokens = p.MaxCompletionTokens
		}
	})
}
