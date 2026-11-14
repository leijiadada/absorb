package errs

import (
	"errors"
	"testing"

	"trpc.group/trpc-go/trpc-agent-go/model"
)

func strptr(s string) *string { return &s }

func TestFromModel_BigModel1210(t *testing.T) {
	t.Parallel()
	re := &model.ResponseError{Code: strptr("1210"), Message: "messages.content.type 参数非法，取值范围 ['text']"}
	norm := FromModel(re, ModelContext{Provider: "bigmodel", ModelID: "glm-4.5-flash", DisplayName: "GLM"})

	if norm.Code != CodeImageUnsupported {
		t.Fatalf("want %s got %s", CodeImageUnsupported, norm.Code)
	}
	if norm.Cause == nil {
		t.Fatal("Cause 应包装原始错误")
	}
	// #5：Unwrap 链应可被 errors.Is 命中
	if !errors.Is(norm, re) {
		t.Fatal("errors.Is 应能通过 Unwrap 命中原始 re")
	}
}

func TestFromModel_OpenAICompatible(t *testing.T) {
	t.Parallel()
	re := &model.ResponseError{Code: strptr("invalid_api_key")}
	norm := FromModel(re, ModelContext{Provider: "openai"})
	if norm.Code != CodeAuthInvalid {
		t.Fatalf("want %s got %s", CodeAuthInvalid, norm.Code)
	}
}

// TestFromModel_BigModelRealHTTPSamples 用真实 GLM 端点返回的错误文本（框架把
// {"error":{...}} 剥成扁平 {"code":...} 塞进 Message、Code 为 nil）验证端到端映射：
// normalize 还原 code → bigModelMapper 命中正确稳定码，而非掉进回退层。
func TestFromModel_BigModelRealHTTPSamples(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string // 框架给的 ResponseError.Message（Code=nil）
		want Code
	}{
		{"1210_image", `POST "https://open.bigmodel.cn/api/paas/v4/chat/completions": 400 Bad Request {"code":"1210","message":"messages.content.type 参数非法，取值范围 ['text']"}`, CodeImageUnsupported},
		{"1211_model_not_found", `POST "https://open.bigmodel.cn/api/paas/v4/chat/completions": 400 Bad Request {"code":"1211","message":"模型不存在，请检查模型代码。"}`, CodeModelNotFound},
		{"401_auth", `POST "https://open.bigmodel.cn/api/paas/v4/chat/completions": 401 Unauthorized {"code":"401","message":"令牌已过期或验证不正确"}`, CodeAuthInvalid},
		{"1301_content", `400 Bad Request {"code":"1301","message":"系统检测到输入或生成内容可能包含不安全或敏感内容"}`, CodeContentPolicy},
		{"1261_context", `400 Bad Request {"code":"1261","message":"Prompt 超长"}`, CodeContextTooLong},
		{"1302_ratelimit", `429 Too Many Requests {"code":"1302","message":"您的账户已达到速率限制，请您控制请求频率"}`, CodeRateLimited},
		{"1220_permission", `403 Forbidden {"code":"1220","message":"您无权访问 xxx"}`, CodePermission},
		{"1113_quota", `429 Too Many Requests {"code":"1113","message":"您的账户已欠费，请充值后重试"}`, CodeQuotaExceeded},
		{"1234_network", `500 Internal Server Error {"code":"1234","message":"网络错误，错误 id：abc"}`, CodeNetwork},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			re := &model.ResponseError{Message: c.raw} // Code=nil（真实运行时如此）
			norm := FromModel(re, ModelContext{Provider: "bigmodel", ModelID: "glm-4.5-flash", DisplayName: "GLM"})
			if norm.Code != c.want {
				t.Fatalf("want %s got %s (cause=%v)", c.want, norm.Code, norm.Cause)
			}
		})
	}
}

// DeepSeek 402 欠费的 code 恒为 invalid_request_error（真实抓包），必须按 type/消息
// 映射为额度不足——不得落进 bad_request 误报「请求格式有误」（2026-11-10 实测事故）。
func TestFromModel_DeepSeek402InsufficientBalance(t *testing.T) {
	t.Parallel()
	re := &model.ResponseError{
		Message: `POST "https://api.deepseek.com/chat/completions": 402 Payment Required {"message":"Insufficient Balance (request_id: d1fc66c0)","type":"insufficient_balance_error","param":null,"code":"invalid_request_error"}`,
	}
	norm := FromModel(re, ModelContext{Provider: "deepseek", ModelID: "deepseek-flash", DisplayName: "DeepSeek"})
	if norm.Code != CodeQuotaExceeded {
		t.Fatalf("want %s got %s (cause=%v)", CodeQuotaExceeded, norm.Code, norm.Cause)
	}
}

func TestFromModel_FallbackTimeout(t *testing.T) {
	t.Parallel()
	re := &model.ResponseError{Message: "context deadline exceeded (Client.Timeout exceeded)"}
	norm := FromModel(re, ModelContext{Provider: "unknown"})
	if norm.Code != CodeTimeout {
		t.Fatalf("want %s got %s", CodeTimeout, norm.Code)
	}
}

func TestFromError_Network(t *testing.T) {
	t.Parallel()
	norm := FromError(errors.New("dial tcp: no such host"), ModelContext{})
	if norm.Code != CodeNetwork {
		t.Fatalf("want %s got %s", CodeNetwork, norm.Code)
	}
}

// TestFromFinishReason 锁定流式终止原因的归并映射（对接 OpenAI / DeepSeek 官方枚举）：
// content_filter→content_policy、length→context_too_long、DeepSeek 私有值→unavailable，
// 正常停止(stop/tool_calls/function_call)与未知值返回 nil（当作正常流结束）。
func TestFromFinishReason(t *testing.T) {
	t.Parallel()
	cases := []struct {
		reason string
		want   Code // "" 表示应返回 nil（正常终止）
	}{
		{"content_filter", CodeContentPolicy},
		{"length", CodeContextTooLong},
		{"insufficient_system_resource", CodeUnavailable},
		{"aborted", CodeUnavailable},
		{"stop", ""},
		{"tool_calls", ""},
		{"function_call", ""},
		{"", ""},
		{"unknown_reason", ""},
	}
	for _, c := range cases {
		t.Run(c.reason, func(t *testing.T) {
			norm := FromFinishReason(c.reason)
			if c.want == "" {
				if norm != nil {
					t.Fatalf("reason=%q want nil got %+v", c.reason, norm)
				}
				return
			}
			if norm == nil {
				t.Fatalf("reason=%q want code %s got nil", c.reason, c.want)
			}
			if norm.Code != c.want {
				t.Fatalf("reason=%q want %s got %s", c.reason, c.want, norm.Code)
			}
		})
	}
}
