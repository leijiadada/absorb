package chat

// 中断恢复（P1）：会话末事件悬挂 tool_calls 时，/api/ask 自动先走恢复轮再处理用户输入。
// 交互对齐 Trae：无按钮、无协议字段——修复是组装层的例行公事，用户只管说话。
//   - 用户输入"继续类"（isContinueLike）→ 仅恢复轮（补执行挂起工具 + 续答），收工
//   - 用户输入新问题 → 恢复轮修复现场后续答，再正常跑一轮回答问题（SSE phase 事件分消息）
// 框架约束（v1.11.2 源码验证）：WithResume 只能配空消息——真实用户消息一旦落库，
// 末事件不再是 tool_calls，resume 检查必然 miss。故"补执行 + 答新题"必须拆两阶段编排。

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/runner"
	"trpc.group/trpc-go/trpc-agent-go/session"

	expand "absorb-agent/tool/expand_context"
	recall "absorb-agent/tool/recall_history"

	"absorb-agent/internal/errs"
	"absorb-agent/internal/kind"
	"absorb-agent/internal/log"
	"absorb-agent/internal/store"
)

// continueLikeReplies "继续类"输入白名单：命中 → 恢复轮续答即视为完成本轮，不再跑
// 用户问题轮（省一次 LLM 调用）。漏命中无害——恢复轮续答后，用户输入作为正常轮再跑，
// 模型面对已续答的上下文会自然收尾。大小写/标点不敏感。
var continueLikeReplies = map[string]struct{}{
	"继续": {}, "继续吧": {}, "请继续": {}, "继续说": {}, "继续讲": {},
	"接着说": {}, "接着讲": {}, "接着写": {}, "接着回答": {}, "继续生成": {},
	"继续回答": {}, "把话说完": {}, "说完": {}, "继续说下去": {},
	"continue": {}, "go on": {}, "go ahead": {}, "keep going": {}, "keepgoon": {},
}

// isContinueLike 判断用户输入是否为"继续类"：去空白与中英文标点后小写比对白名单。
func isContinueLike(question string) bool {
	norm := strings.ToLower(strings.TrimSpace(question))
	norm = strings.Trim(norm, "。．.!！?？~～，, ")
	if norm == "" {
		return true // 空输入视同"继续"（唯一安全的默认：恢复续答，不猜测新问题）
	}
	_, ok := continueLikeReplies[norm]
	return ok
}

// resumeIfDangling 中断恢复编排入口（从 AskHandler 调用）：会话末事件悬挂 tool_calls
// 时先跑恢复轮，再按用户输入决定是否继续正常轮。返回 true 表示本轮已在恢复路径内
// 终结（continue-like 完成 / 恢复失败已下发 error），调用方应直接 return。
// 检测失败按无悬挂降级：只丢恢复能力，不影响正常问答。
func resumeIfDangling(
	ctx context.Context,
	r runner.Runner,
	sessionSvc *store.SessionService,
	app, user, sessionID, question string,
	reg *RunRegistry,
	appCtx errs.ModelContext,
	span trace.Span,
	w http.ResponseWriter,
	flusher http.Flusher,
) bool {
	dangling, derr := sessionSvc.HasDanglingToolCall(ctx, app, user, sessionID)
	if derr != nil {
		// 检测失败按无悬挂降级：正常轮仍可用，仅恢复能力缺失
		log.Warnf("[Absorb] dangling tool call check failed, skip resume (session=%s): %v", sessionID, derr)
		return false
	}
	if !dangling {
		return false
	}
	span.SetAttributes(attribute.Bool("resume_pending_tools", true))
	reqIDA := resumePhase(ctx, r, sessionSvc, app, user, sessionID, reg, appCtx, w, flusher)
	if reqIDA == "" {
		// 恢复失败：SSE error 已下发。悬挂仍在，带着残缺历史跑新问题可能 400，不降级
		return true
	}
	if isContinueLike(question) {
		// "继续"类输入：恢复轮续答即完成本轮；requestId 随 done 下发（知识入口挂恢复轮）
		writeSSE(w, map[string]any{"done": true, "requestId": reqIDA, "resumed": true})
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	// 新问题：phase 边界事件让前端开启新消息气泡，随后正常轮照旧
	writeSSE(w, map[string]any{"type": "phase"})
	if flusher != nil {
		flusher.Flush()
	}
	return false
}

// resumePhase 恢复轮（Phase A）：空消息 + WithResume(true)，框架在 LLM cycle 前补执行
// 悬挂 tool_calls（确定性补执行：沿用落库参数与同一套工具/回调），结果落库后自动续答。
// token/reasoning 照常经 SSE 流式下发；IsFinalResponse 不发 done（本轮不结束请求）。
// 返回恢复轮 requestID（continue-like 路径随 done 下发，作知识保存 originEventId）；
// 失败返回空串（SSE error 已下发）——恢复失败时悬挂仍在，带着残缺历史跑新问题可能
// 直接 400，故不降级继续 Phase B。
func resumePhase(
	ctx context.Context,
	r runner.Runner,
	sessionSvc *store.SessionService,
	app, user, sessionID string,
	reg *RunRegistry,
	appCtx errs.ModelContext,
	w http.ResponseWriter,
	flusher http.Flusher,
) string {
	// 工具状态注入与被中断轮对齐（恢复轮可能补执行 expand_context / recall_history）：
	// scope 按会话历史继承，页面缓存与参考材料按该 scope 读回，recall 拿同一套摘要 cutoff。
	scopeURL, _ := sessionSvc.RecentPageURL(ctx, app, user, sessionID)
	filterKey := store.ScopeID(scopeURL)
	if filterKey == "" {
		filterKey = kind.ScopeGeneral
	}
	pageText, perr := sessionSvc.LoadPageText(ctx, app, user, sessionID, filterKey)
	if perr != nil {
		log.Warnf("[Absorb] resume load page cache failed (session=%s): %v", sessionID, perr)
	}
	refMat, _ := sessionSvc.RecentReferenceMaterial(ctx, app, user, sessionID, filterKey)
	recallKey := session.Key{AppName: app, UserID: user, SessionID: sessionID}
	runCtx := expand.WithPageSearchState(ctx, pageText, refMat)
	runCtx = recall.WithRecallState(runCtx, sessionSvc, recallKey, filterKey,
		sessionSvc.SummaryCutoffs(ctx, recallKey))

	requestID := uuid.NewString()
	reg.set(sessionID, requestID)
	defer reg.del(sessionID)

	_, span := otel.Tracer("absorb/chat").Start(ctx, "ask.resume")
	defer span.End()
	span.SetAttributes(
		attribute.String("request_id", requestID),
		attribute.String("session_id", sessionID),
		attribute.String("filter_key", filterKey),
	)

	eventChan, err := r.Run(runCtx, user, sessionID, model.Message{}, []agent.RunOption{
		agent.WithRequestID(requestID),
		agent.WithDetachedCancel(true),
		agent.WithMaxRunDuration(maxRunDuration),
		agent.WithEventFilterKey(filterKey),
		agent.WithResume(true),
	}...)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		log.Errorf("[Absorb] resume run start failed (session=%s): %v", sessionID, err)
		writeSSE(w, map[string]any{"type": "error", "message": "恢复上次中断的回答失败: " + err.Error()})
		if flusher != nil {
			flusher.Flush()
		}
		return ""
	}

	for evt := range eventChan {
		if evt.Error != nil {
			norm := errs.FromModel(evt.Error, appCtx)
			span.SetStatus(codes.Error, norm.Message)
			log.Errorf("[Absorb] resume model error | provider=%s code=%s session=%s",
				appCtx.Provider, norm.Code, sessionID)
			writeSSE(w, map[string]any{"type": "error", "code": string(norm.Code), "message": norm.Message})
			if flusher != nil {
				flusher.Flush()
			}
			return ""
		}
		if evt.Response != nil && len(evt.Response.Choices) > 0 {
			c := evt.Response.Choices[0]
			if rc := c.Delta.ReasoningContent; rc != "" {
				writeSSE(w, map[string]any{"type": "reasoning", "content": rc})
				if flusher != nil {
					flusher.Flush()
				}
			}
			if content := c.Delta.Content; content != "" {
				writeSSE(w, map[string]any{"type": "token", "content": content})
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
		// IsFinalResponse 不发 done：恢复轮只是 Phase A，后续还有用户问题轮
		//（continue-like 由调用方补发 done）。继续 drain 到 runner 落库关流。
	}
	return requestID
}
