package chat

// /api/ask/answer：ask_user 提问的用户回答端点（官方 Human-in-the-Loop 范式）。
// 用户回答组装成更新 JSON（tool/askuser.BuildAnswerUpdate）作为新 user message
// 发起续答 run；会话历史中已有 pending 工具结果，序列合法，模型据此继续回答。
// 流式语义与 /api/ask 一致（token/reasoning/done），前端把续答渲染进同一气泡。

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/runner"
	"trpc.group/trpc-go/trpc-agent-go/session"

	askuser "absorb-agent/tool/askuser"

	"absorb-agent/internal/errs"
	"absorb-agent/internal/kind"
	"absorb-agent/internal/log"
	"absorb-agent/internal/store"
)

// AnswerHandler 处理 POST /api/ask/answer：body {sessionId, ticketId, answer}。
// 校验失败返回 JSON 错误（400）；run 启动后走 SSE（token/reasoning/done）。
func AnswerHandler(r runner.Runner, ps PromptStrategy, sessionSvc *store.SessionService, app, user string, reg *RunRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		// CORS 响应头（OPTIONS 预检由路由层注册）
		w.Header().Set("Access-Control-Allow-Origin", "*")
		var body struct {
			SessionID string `json:"sessionId"`
			TicketID  string `json:"ticketId"`
			Answer    string `json:"answer"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"code": 1, "message": "请求体解析失败: " + err.Error()})
			return
		}
		if strings.TrimSpace(body.SessionID) == "" || strings.TrimSpace(body.Answer) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"code": 1, "message": "sessionId 与 answer 不能为空"})
			return
		}

		ctx := req.Context()
		// L1 校验：会话不存在/已清理时不开续答（与知识保存的来源校验同口径）。
		// GetSession 对不存在 key 会自动建空会话，空事件序列同样判缺失。
		key := session.Key{AppName: app, UserID: user, SessionID: body.SessionID}
		sess, gerr := sessionSvc.GetSession(ctx, key)
		if gerr != nil || sess == nil || len(sess.Events) == 0 {
			log.Warnf("[Absorb] answer session missing (session=%s): %v", body.SessionID, gerr)
			writeJSON(w, http.StatusBadRequest, map[string]any{"code": 1, "message": "会话不存在或已清理，无法继续回答"})
			return
		}

		// scope 继承：pending 提问归属页面的 scope，事件过滤键与问答链路同源
		scopeURL, _ := sessionSvc.RecentPageURL(ctx, app, user, body.SessionID)
		filterKey := store.ScopeID(scopeURL)
		if filterKey == "" {
			filterKey = kind.ScopeGeneral
		}

		ctx, span := otel.Tracer("absorb/chat").Start(ctx, "ask.answer")
		defer span.End()
		requestID := uuid.NewString()
		reg.set(body.SessionID, requestID)
		defer reg.del(body.SessionID)
		span.SetAttributes(
			attribute.String("request_id", requestID),
			attribute.String("session_id", body.SessionID),
			attribute.String("filter_key", filterKey),
		)

		update := askuser.BuildAnswerUpdate(body.TicketID, body.Answer)
		eventChan, err := r.Run(ctx, user, body.SessionID, model.NewUserMessage(update), []agent.RunOption{
			agent.WithRequestID(requestID),
			agent.WithDetachedCancel(true),
			agent.WithMaxRunDuration(maxRunDuration),
			agent.WithEventFilterKey(filterKey),
		}...)
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]any{"code": 1, "message": "续答启动失败: " + err.Error()})
			return
		}

		// SSE 头（校验失败路径未写过头，此处才开始事件流）
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)

		appCtx := errs.ModelContext{}
		if mc, merr := sessionSvc.GetDefaultModelConfig(ctx, app); merr == nil && mc != nil {
			appCtx = errs.ModelContext{Provider: mc.Provider, ModelID: mc.ModelID, DisplayName: mc.DisplayName}
		}

		// 更新 JSON 回执由提示词约束为内部回执（禁止复述），本端点不对其做流式过滤
		sentDone := false
		for evt := range eventChan {
			if evt.Error != nil {
				norm := errs.FromModel(evt.Error, appCtx)
				span.SetStatus(codes.Error, norm.Message)
				log.Errorf("[Absorb] answer model error | provider=%s code=%s session=%s",
					appCtx.Provider, norm.Code, body.SessionID)
				writeSSE(w, map[string]any{"type": "error", "code": string(norm.Code), "message": norm.Message})
				if flusher != nil {
					flusher.Flush()
				}
				return
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
			if !sentDone && evt.IsFinalResponse() {
				writeSSE(w, map[string]any{"done": true, "requestId": requestID})
				if flusher != nil {
					flusher.Flush()
				}
				sentDone = true
				// 不 break：继续 drain 到 runner 完成事件落库并关闭 channel
			}
		}
		if !sentDone {
			writeSSE(w, map[string]any{"done": true, "requestId": requestID})
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}
