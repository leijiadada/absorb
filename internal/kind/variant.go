package kind

// Variant 标识 OpenAI 协议下的 API 方言（影响思考字段格式等厂商差异）。
// 空串 "" 表示由框架按 base_url 自动推断。
type Variant string

const (
	VariantGLM      Variant = "glm"
	VariantDeepSeek Variant = "deepseek"
	VariantKimi     Variant = "kimi"
	VariantMiniMax  Variant = "minimax"
	VariantQwen     Variant = "qwen"
)

// Valid 报告 v 是否为已知方言（空串 "" 代表自动推断，也视为合法）。
func (v Variant) Valid() bool {
	switch v {
	case "", VariantGLM, VariantDeepSeek, VariantKimi, VariantMiniMax, VariantQwen:
		return true
	default:
		return false
	}
}
