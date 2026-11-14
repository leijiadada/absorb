// Package errs 跨模型/厂商的通用错误归一化模块。
//
// 职责：把厂商原始错误(*model.ResponseError)翻成稳定错误码 Code + 中性兜底文案，
// 技术细节仅留日志、不外发（Norm.Cause 保留原始错误，经 Unwrap 支持 errors.As/Is）。
// mapper 只翻译不日志，由调用方统一落一次技术日志；稳定 Code 作为日志聚合键，
// 变化量作结构化字段。
package errs

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"absorb-agent/internal/kind"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

// Code 跨模型/厂商的稳定错误码，作为服务端↔前端契约（JSON 安全字符串）。
type Code string

const (
	// 用户输入/配置类：前端可给可操作提示
	CodeImageUnsupported Code = "model.image_unsupported"
	CodeAuthInvalid      Code = "model.auth_invalid"
	CodePermission       Code = "model.permission_denied"
	CodeRateLimited      Code = "model.rate_limited"
	CodeQuotaExceeded    Code = "model.quota_exceeded"
	CodeContextTooLong   Code = "model.context_too_long"
	CodeModelNotFound    Code = "model.not_found"
	CodeContentPolicy    Code = "model.content_policy"
	CodeBadRequest       Code = "model.bad_request"
	// 系统/服务类：前端给“请稍后重试”中性文案
	CodeTimeout     Code = "model.timeout"
	CodeNetwork     Code = "model.network"
	CodeUnavailable Code = "model.unavailable"
	CodeInternal    Code = "agent.internal"
)

// ModelContext 映射所需运行时上下文（不含密钥等敏感字段）。
type ModelContext struct {
	Provider    string
	ModelID     string
	DisplayName string
}

// Norm 归一化后的错误：稳定码 + 中性兜底文案 + 技术原因(仅日志)。
type Norm struct {
	Code    Code
	Message string
	Cause   error
}

func (e *Norm) Error() string { return string(e.Code) + ": " + e.Message }
func (e *Norm) Unwrap() error { return e.Cause }

// Mapper 把厂商原始错误翻成 Norm；未命中返回 nil（交给回退层）。
type Mapper func(re *model.ResponseError, mc ModelContext) *Norm

// defaultMessages 各稳定码的中性/英文兜底文案（前端应优先用自身 i18n 覆盖）。
var defaultMessages = map[Code]string{
	CodeImageUnsupported: "model does not support image input",
	CodeAuthInvalid:      "API key invalid or unauthorized",
	CodePermission:       "no permission for this model or API",
	CodeRateLimited:      "rate limited or quota exceeded",
	CodeQuotaExceeded:    "account quota exceeded",
	CodeContextTooLong:   "content exceeds model context limit",
	CodeModelNotFound:    "model not found",
	CodeContentPolicy:    "request or response blocked by content policy",
	CodeBadRequest:       "bad request to model",
	CodeTimeout:          "request timeout",
	CodeNetwork:          "cannot connect to service",
	CodeUnavailable:      "service unavailable",
	CodeInternal:         "internal error",
}

func defaultMessage(c Code) string {
	if m, ok := defaultMessages[c]; ok {
		return m
	}
	return "unknown error"
}

// registry 按 kind.Variant（API 方言）分组，各家方言自管一张表，
// 实现"不同模型不同错误描述与错误码映射"。
// 按方言而非厂商名分组，避免与 Provider 枚举混淆；
// 未识别方言（含 "openai"）统一由空串 key 兜底为 OpenAI 兼容处理。
var registry = map[kind.Variant]Mapper{
	kind.VariantGLM:      bigModelMapper,
	kind.VariantDeepSeek: openAICompatibleMapper,
	kind.VariantKimi:     openAICompatibleMapper,
	kind.VariantMiniMax:  openAICompatibleMapper,
	kind.VariantQwen:     openAICompatibleMapper,
	"":                   openAICompatibleMapper, // 未识别/自动推断方言，按 OpenAI 兼容处理（原 "openai" key）
}

// FromModel 把模型返回的 *model.ResponseError 归一化。
// 优先按厂商对应的 API 方言查表，未命中再走通用 HTTP/网络回退层。
//
// 框架（model/openai）仅在「HTTP 200 + 内嵌 error body」时填 ResponseError.Code；
// 真实 4xx/5xx 走流式/非流式错误路径，只把原始 HTTP 错误文本（含 body 的 JSON）放进
// Message、Code 恒为空。故先用 normalize 从 Message 恢复有效 code/message，再查表。
func FromModel(re *model.ResponseError, mc ModelContext) *Norm {
	if re == nil {
		return &Norm{Code: CodeInternal, Message: defaultMessage(CodeInternal)}
	}
	code, message := normalize(re)
	raw := re.Message // 原始 HTTP 错误文本，留作日志
	// 把恢复的 code/message 就地写回原始 re：re 仅在本次翻译中使用、调用方不再复用，
	// 这样 provider 表与回退层都能读到恢复值，且 errors.Is(norm, re) 仍成立
	// （mapper 内部以 %w 包装的正是这个 re 指针）。
	if code != "" && re.Code == nil {
		re.Code = &code
	}
	if message != "" && message != raw {
		re.Message = message
	}
	if m, ok := registry[kind.Provider(mc.Provider).Variant()]; ok {
		if ae := m(re, mc); ae != nil {
			return ae
		}
	}
	return fallback(re, mc, fmt.Errorf("%s provider code=%v type=%v raw=%q: %w",
		mc.Provider, code, re.Type, raw, re))
}

// FromError 把非模型错误(超时/网络/未配置等)归一化，复用回退层。
func FromError(err error, mc ModelContext) *Norm {
	if err == nil {
		return &Norm{Code: CodeInternal, Message: defaultMessage(CodeInternal)}
	}
	return fallback(&model.ResponseError{Message: err.Error()}, mc, err)
}

// norm 构造带稳定码与包装 Cause 的 Norm（provider 表与回退层共用）。
func norm(code Code, cause error) *Norm {
	return &Norm{Code: code, Message: defaultMessage(code), Cause: cause}
}

// FromFinishReason 把流式推理的终止原因(finish_reason)翻成 Norm。
//
// SSE 推理中途异常终止时，厂商不在 HTTP 错误体返回，而是把原因写在 200 流的
// finish_reason 里（GLM 官方文档明确）。这类值与 OpenAI 规范基本对齐，故用一份
// 共享映射；个别厂商私有值可在此覆盖。正常终止(stop/tool_calls/function_call/空)
// 返回 nil。请求级错误（图片不支持、鉴权、模型不存在等）走 HTTP 错误体，由
// FromModel 处理，不在此列。
func FromFinishReason(reason string) *Norm {
	switch reason {
	case "content_filter":
		return norm(CodeContentPolicy, fmt.Errorf("stream finish_reason=content_filter"))
	case "length":
		return norm(CodeContextTooLong, fmt.Errorf("stream finish_reason=length"))
	// DeepSeek 私有值（见 api-docs.deepseek.com 枚举）：服务侧资源不足 / 请求被中止，
	// 归并为服务不可用。
	case "insufficient_system_resource", "aborted":
		return norm(CodeUnavailable, fmt.Errorf("stream finish_reason=%s", reason))
	case "function_call", "tool_calls", "stop", "":
		return nil // 正常终止，非错误
	}
	return nil // 未知原因，交上层当作正常流结束处理
}

// fallback 通用层：基于 provider code / type / 消息特征映射到稳定码。
// 仅作最后兜底；优先由各 provider 表精确命中。关键词匹配避免裸数字(如 "401")以防误伤。
func fallback(re *model.ResponseError, mc ModelContext, cause error) *Norm {
	msg := strings.ToLower(re.Message)
	code := derefCode(re.Code)
	switch {
	case code == "invalid_api_key" || code == "authentication_error" ||
		contains(msg, "invalid api key", "unauthorized", "authentication failed"):
		return norm(CodeAuthInvalid, cause)
	case code == "rate_limit_exceeded" || code == "rate_limit_error" ||
		contains(msg, "rate limit", "too many requests", "quota", "quota exceeded"):
		return norm(CodeRateLimited, cause)
	case contains(msg, "insufficient", "balance", "quota exceeded", "额度"):
		return norm(CodeQuotaExceeded, cause)
	case code == "context_length_exceeded" ||
		contains(msg, "context length", "maximum context", "too long", "上下文长度"):
		return norm(CodeContextTooLong, cause)
	case code == "model_not_found" || contains(msg, "model not found", "does not exist"):
		return norm(CodeModelNotFound, cause)
	case code == "invalid_request_error" || code == "bad_request" ||
		contains(msg, "invalid request"):
		return norm(CodeBadRequest, cause)
	case contains(msg, "timeout", "deadline", "context deadline") || code == "timeout":
		return norm(CodeTimeout, cause)
	case contains(msg, "connection refused", "no such host", "dial tcp", "proxyconnect", "network is unreachable"):
		return norm(CodeNetwork, cause)
	case contains(msg, "internal server error", "service unavailable", "bad gateway", "gateway timeout"):
		return norm(CodeUnavailable, cause)
	}
	return norm(CodeInternal, cause)
}

// normalize 从框架给的 *model.ResponseError 提取“有效”的厂商 code 与 message。
//
// 框架（model/openai）构造 ResponseError 有三条路径：
//  1. HTTP 200 + 内嵌 error body：会解析并填 Code；
//  2. 流式错误（真实 4xx/5xx 主要走这条）：仅填 Message，Code 恒为空；
//  3. 非流式错误：同上，仅填 Message。
//
// 因此真实错误里 Code 几乎总是空，厂商码被埋进 Message 的文本中
// （形如 `POST "...": 400 Bad Request {"code":"1210","message":"..."}`）。
// 这里在 Code 为空时，尝试从 Message 解析内嵌的厂商错误 JSON，还原 code/message，
// 供各 provider 表与回退层使用。
func normalize(re *model.ResponseError) (code, message string) {
	code, message = derefCode(re.Code), re.Message
	if code != "" {
		return code, message
	}
	if c, m, ok := extractEmbeddedError(re.Message); ok {
		code = c
		if m != "" {
			message = m
		}
	}
	return code, message
}

// extractEmbeddedError 从形如 `... 400 Bad Request {"code":"1210","message":"..."}`
// 的错误文本中解析内嵌的厂商错误 JSON，提取 code / message 字段；
// 同时兼容被 {"error":{...}} 包裹的形态。找不到合法结构时 ok=false。
func extractEmbeddedError(raw string) (code, message string, ok bool) {
	start := strings.Index(raw, "{")
	if start < 0 {
		return "", "", false
	}
	var obj struct {
		Code    any    `json:"code"`
		Message string `json:"message"`
		Error   struct {
			Code    any    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	// 先试首个 '{'（真实错误多为 `... 400 Bad Request {json}`）；
	// 失败再试最后一个 '{'（兼容 JSON 不在尾部的情况）。
	if err := json.Unmarshal([]byte(raw[start:]), &obj); err != nil {
		last := strings.LastIndex(raw, "{")
		if last < 0 || last == start {
			return "", "", false
		}
		if err2 := json.Unmarshal([]byte(raw[last:]), &obj); err2 != nil {
			return "", "", false
		}
	}
	c, m := "", ""
	if obj.Code != nil {
		c = anyCode(obj.Code)
	}
	if obj.Error.Code != nil {
		c = anyCode(obj.Error.Code) // 包裹形态优先取 error.code
	}
	if obj.Message != "" {
		m = obj.Message
	}
	if obj.Error.Message != "" {
		m = obj.Error.Message
	}
	if c == "" && m == "" {
		return "", "", false
	}
	return c, m, true
}

// anyCode 把 JSON 的 code 字段（string 或 number）规范成字符串。
func anyCode(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatInt(int64(t), 10)
	default:
		return ""
	}
}

func derefCode(c *string) string {
	if c == nil {
		return ""
	}
	return *c
}

func contains(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
