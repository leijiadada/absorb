// Package kind 集中存放跨包共享的"类型枚举 / 常量"。
//
// 只收两类东西：
//  1. 表示某类事物"种类"的枚举（如服务商 Provider、API 协议方言 Variant）；
//  2. 需要被多处引用、且代表类别的常量。
//
// 领域私有的常量/状态应留在各自包，避免本包变成 consts 大杂烩。
// Provider（厂商）与 Variant（API 方言）是两套独立枚举，不要混用；
// "openai" 只是 OpenAI 兼容方言的兜底（空串 Variant），不作为 Provider 枚举值。
//
// 本包按领域拆分文件：provider.go（服务商）、variant.go（API 方言）、
// scope.go（会话 scope 身份类别）、summary.go（摘要回溯提示契约）。
package kind

import (
	"net/url"
	"strings"
)

// Provider 标识大模型服务商，按 base_url 主机推断，识别不出为 Custom。
type Provider string

// providerHosts 服务商域名匹配表（Contains 子串语义，按声明顺序命中）。
// 新增服务商在此登记一行，推断与枚举即同时生效，避免枚举/匹配两处真相源。
var providerHosts = []struct {
	host     string
	provider Provider
}{
	{"bigmodel.cn", ProviderBigModel},
	{"deepseek.com", ProviderDeepSeek},
	{"moonshot", ProviderKimi},
	{"dashscope.aliyuncs.com", ProviderDashScope},
	{"siliconflow.cn", ProviderSiliconFlow},
	{"volces.com", ProviderVolces},
	{"hunyuan", ProviderHunyuan},
	{"minimax", ProviderMiniMax},
	{"openrouter.ai", ProviderOpenRouter},
}

// ProviderFromURL 按 base_url 主机推断服务商，识别不出返回 ProviderCustom。
func ProviderFromURL(baseURL string) Provider {
	host := strings.ToLower(baseURL)
	if u, err := url.Parse(strings.TrimSpace(baseURL)); err == nil && u.Host != "" {
		host = strings.ToLower(u.Host)
	}
	for _, m := range providerHosts {
		if strings.Contains(host, m.host) {
			return m.provider
		}
	}
	return ProviderCustom
}

const (
	ProviderBigModel    Provider = "bigmodel"
	ProviderDeepSeek    Provider = "deepseek"
	ProviderKimi        Provider = "kimi"
	ProviderDashScope   Provider = "dashscope"
	ProviderSiliconFlow Provider = "siliconflow"
	ProviderVolces      Provider = "volces"
	ProviderHunyuan     Provider = "hunyuan"
	ProviderMiniMax     Provider = "minimax"
	ProviderOpenRouter  Provider = "openrouter"
	// ProviderCustom 识别不出服务商时的兜底值。
	ProviderCustom Provider = "custom"
)

// Valid 报告 p 是否为已知服务商（含 Custom 兜底）。
func (p Provider) Valid() bool {
	switch p {
	case ProviderBigModel, ProviderDeepSeek, ProviderKimi,
		ProviderDashScope, ProviderSiliconFlow, ProviderVolces,
		ProviderHunyuan, ProviderMiniMax, ProviderOpenRouter, ProviderCustom:
		return true
	default:
		return false
	}
}

// Variant 返回服务商对应的 API 方言。
// 未知服务商返回空串，交由框架按 base_url 自动推断。
func (p Provider) Variant() Variant {
	switch p {
	case ProviderBigModel:
		return VariantGLM
	case ProviderDeepSeek:
		return VariantDeepSeek
	case ProviderKimi:
		return VariantKimi
	case ProviderMiniMax:
		return VariantMiniMax
	default:
		return ""
	}
}
