package errs

import (
	"fmt"

	"trpc.group/trpc-go/trpc-agent-go/model"
)

// bigModelMapper 智谱 GLM / bigmodel 端点错误码映射。
//
// 数据来源：官方文档 https://docs.bigmodel.cn/cn/api/api-code.md 与端点实测。
//   - 错误体形如 {"error":{"code":"1211","message":"..."}}，code 为字符串；
//     框架(model/openai)会把 wrapper 剥掉后把 {"code":...} 塞进 ResponseError.Message，
//     故 errs.normalize 能还原 code（见 errs.go）。
//   - 鉴权失败真实端点返回 code="401"（把 HTTP 状态当业务码），与文档 1000/1001 并存，二者都认。
//   - 图片不支持(code=1210)走 HTTP 400 错误体而非 finish_reason。
//   - 生成中途被安全拦截等异常走流的 finish_reason（见 errs.FromFinishReason），本表不覆盖。
func bigModelMapper(re *model.ResponseError, mc ModelContext) *Norm {
	code := derefCode(re.Code)
	switch code {
	// ── 鉴权 ───────────────────────────────────────────────
	case "401", "1000", "1001", "1003", "1005":
		return norm(CodeAuthInvalid, fmt.Errorf("bigmodel code=%s (auth): %w", code, re))

	// ── 权限（403/套餐未开放/Key 场景不匹配）──────────────
	case "1220", "1311", "1315":
		return norm(CodePermission, fmt.Errorf("bigmodel code=%s (permission): %w", code, re))

	// ── 额度/欠费 ─────────────────────────────────────────
	case "1113", "1163", "1309", "1314",
		"1316", "1317", "1318", "1319", "1320", "1321":
		return norm(CodeQuotaExceeded, fmt.Errorf("bigmodel code=%s (quota/balance): %w", code, re))

	// ── 限流/并发 ─────────────────────────────────────────
	case "1302", "1305", "1308", "1310", "1313":
		return norm(CodeRateLimited, fmt.Errorf("bigmodel code=%s (rate limit): %w", code, re))

	// ── 模型/API 不存在 ───────────────────────────────────
	case "1211", "1222":
		return norm(CodeModelNotFound, fmt.Errorf("bigmodel code=%s (model/api not found): %w", code, re))
	case "1221":
		return norm(CodeUnavailable, fmt.Errorf("bigmodel code=%s (api offline): %w", code, re))

	// ── 上下文超限 ────────────────────────────────────────
	case "1261":
		return norm(CodeContextTooLong, fmt.Errorf("bigmodel code=%s (prompt too long): %w", code, re))

	// ── 内容安全/审核拦截 ─────────────────────────────────
	case "1301":
		return norm(CodeContentPolicy, fmt.Errorf("bigmodel code=%s (content policy): %w", code, re))

	// ── 图片/多模态不支持（仅纯文本模型收到图片时）────────
	case "1210":
		// messages.content.type 参数非法，取值范围 ['text']：
		// 即把图片/image_url 发给了仅支持纯文本的模型。
		return norm(CodeImageUnsupported,
			fmt.Errorf("bigmodel code=1210 (image not supported): %w", re))

	// ── 参数错误（缺参/参数非法/互斥/调用方式不支持）──────
	case "1212", "1213", "1214", "1215":
		return norm(CodeBadRequest, fmt.Errorf("bigmodel code=%s (bad request): %w", code, re))

	// ── 服务端错误 ────────────────────────────────────────
	case "1200", "1230":
		return norm(CodeInternal, fmt.Errorf("bigmodel code=%s (server error): %w", code, re))
	case "1234":
		return norm(CodeNetwork, fmt.Errorf("bigmodel code=%s (network error): %w", code, re))
	}
	return nil // 交回退层
}
