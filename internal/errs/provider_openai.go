package errs

import (
	"fmt"
	"strings"

	"trpc.group/trpc-go/trpc-agent-go/model"
)

// openAICompatibleMapper 适用于 OpenAI 兼容端点（openai/deepseek/kimi 等）。
// 这些厂商的 error.code/type 多为 OpenAI 标准值。
func openAICompatibleMapper(re *model.ResponseError, mc ModelContext) *Norm {
	code := derefCode(re.Code)
	// DeepSeek 欠费（HTTP 402）的 code 恒为 invalid_request_error，仅 type/消息可区分——
	// 必须先于下方按 code 的 bad_request 映射检查，否则欠费被误报成「请求格式有误」。
	if re.Type == "insufficient_balance_error" ||
		contains(strings.ToLower(re.Message), "insufficient balance") {
		return norm(CodeQuotaExceeded, fmt.Errorf("%s code=%s type=%s: %w", mc.Provider, code, re.Type, re))
	}
	switch code {
	case "invalid_api_key", "authentication_error":
		return norm(CodeAuthInvalid, fmt.Errorf("%s code=%s: %w", mc.Provider, code, re))
	case "rate_limit_exceeded", "rate_limit_error":
		return norm(CodeRateLimited, fmt.Errorf("%s code=%s: %w", mc.Provider, code, re))
	case "insufficient_quota", "quota_exceeded":
		return norm(CodeQuotaExceeded, fmt.Errorf("%s code=%s: %w", mc.Provider, code, re))
	case "context_length_exceeded":
		return norm(CodeContextTooLong, fmt.Errorf("%s code=%s: %w", mc.Provider, code, re))
	case "model_not_found":
		return norm(CodeModelNotFound, fmt.Errorf("%s code=%s: %w", mc.Provider, code, re))
	case "invalid_request_error":
		return norm(CodeBadRequest, fmt.Errorf("%s code=%s: %w", mc.Provider, code, re))
	}
	// 部分厂商把类型放在 Type 字段
	switch re.Type {
	case "authentication_error":
		return norm(CodeAuthInvalid, fmt.Errorf("%s type=%s: %w", mc.Provider, re.Type, re))
	case "rate_limit_error":
		return norm(CodeRateLimited, fmt.Errorf("%s type=%s: %w", mc.Provider, re.Type, re))
	case "invalid_request_error":
		return norm(CodeBadRequest, fmt.Errorf("%s type=%s: %w", mc.Provider, re.Type, re))
	}
	return nil // 交回退层
}
