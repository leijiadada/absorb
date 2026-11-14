package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/runner"

	askuser "absorb-agent/tool/askuser"

	"absorb-agent/internal/conf"
	"absorb-agent/internal/errs"
	"absorb-agent/internal/kind"
	"absorb-agent/internal/knowledge"
	"absorb-agent/internal/log"
	"absorb-agent/internal/store"
	"absorb-agent/internal/telemetry/tokentracker"
)

// UserID 会话归属的固定用户标识（单用户桌面形态）。
const UserID = "absorb-user"

// scopeReadable scope 的日志/映射可读形态：有 URL 取归一网址，无 URL 取通用占位。
// 无页面轮次落 kind.ScopeGeneral（不再留空标），理由与定名约束见 kind/scope.go。
func scopeReadable(scopeURL string) string {
	if u := store.NormalizeScopeID(scopeURL); u != "" {
		return u
	}
	return kind.ScopeGeneralDesc
}

// maxRunDuration run 与 SSE 连接解耦（DetachedCancel），不随连接关闭取消；此为 run 最长存活时间
const maxRunDuration = 5 * time.Minute

// RunRegistry 维护 sessionId -> 当前运行中的 requestID 映射，
// 供 /api/cancel 按会话定位并中断对应 run（与 SSE 连接生命周期解耦）。
type RunRegistry struct {
	mu        sync.Mutex
	bySession map[string]string
}

func NewRunRegistry() *RunRegistry {
	return &RunRegistry{bySession: make(map[string]string)}
}

func (rr *RunRegistry) set(sessionID, requestID string) {
	if sessionID == "" || requestID == "" {
		return
	}
	rr.mu.Lock()
	defer rr.mu.Unlock()
	rr.bySession[sessionID] = requestID
}

func (rr *RunRegistry) del(sessionID string) {
	if sessionID == "" {
		return
	}
	rr.mu.Lock()
	defer rr.mu.Unlock()
	delete(rr.bySession, sessionID)
}

func (rr *RunRegistry) get(sessionID string) string {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	return rr.bySession[sessionID]
}

func imageSource(im ImageInput) string {
	if im.URL != "" {
		return im.URL
	}
	return im.DataURL
}

// validateImage 校验单图：URL 仅放行 http/https（防借 url 字段偷塞 base64 或 SSRF）；
// base64 形态校验 mime/大小，URL 形态因远端无法预检而跳过。
func validateImage(im ImageInput) error {
	if u := im.URL; u != "" {
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			return fmt.Errorf("图片链接仅支持 http/https 地址")
		}
		return nil
	}
	allowedMime := map[string]bool{"image/png": true, "image/jpeg": true, "image/webp": true}
	if !allowedMime[im.Mime] {
		return fmt.Errorf("图片格式不支持（仅 png/jpeg/webp）")
	}
	if len(im.DataURL) > 20*1024*1024 {
		return fmt.Errorf("单张图片过大（≤ 20MB）")
	}
	return nil
}

// AskHandler 处理 /api/ask：调 runner 跑 agent，把流式 chunk 包成 SSE 推给扩展；
// sessionSvc 用于追问时查询历史参考材料重申；app/user 是会话定位维度。
// ks 是 M3.1 关联检索依赖（nil = 关闭关联提示）：回答完成后检索相关个人知识，
// 有命中在 done 前下发 associations 事件（低干扰增量，失败/零命中均静默）。
// wsFn 返回工作集上下文配置（运行时经闭包读取，支持配置热切换）；传 nil 视为默认启用。
// 请求体到 runner 入参的组装统一走 PrepareInvocation（评估链路共用同一实现）。
func AskHandler(r runner.Runner, ps PromptStrategy, sessionSvc *store.SessionService, app, user string, reg *RunRegistry, wsFn func() conf.WorkingSetConfig, ks *knowledge.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		// CORS 响应头（OPTIONS 预检与方法校验由路由层承担，见 bridge.CorsPreflight 与 "POST /api/ask" 模式）
		w.Header().Set("Access-Control-Allow-Origin", "*")

		var body InvocationInput
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeSSE(w, map[string]any{"type": "error", "message": "请求体解析失败: " + err.Error()})
			return
		}

		// 根 span 贯穿整轮问答：覆盖多模态校验 → 组装（PrepareInvocation，ctx 贯通
		// 框架埋点）→ runner（gen_ai.* 子 span 挂其下成树）→ SSE drain（含完成
		// 事件落库）。defer End 实际触发于 handler 返回，即落库完成后。
		ctx, span := otel.Tracer("absorb/chat").Start(req.Context(), "ask")
		defer span.End()
		span.SetAttributes(
			attribute.String("app_name", app),
			attribute.String("user_id", user),
			attribute.String("question", truncateRunes(body.Question, 2000)),
			attribute.Bool("has_selection", body.Context != nil && strings.TrimSpace(body.Context.Text) != ""),
		)
		if body.Page != nil {
			span.SetAttributes(attribute.String("scope_url", body.Page.URL))
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, _ := w.(http.Flusher)

		// 多模态前置校验（仅携带图片时）：数量 / 格式 / 大小 / 模型视觉能力。
		// 不在此预检 GLM——GLM 端点会返回 1210，由下方模型调用错误统一提示。
		if len(body.Images) > 0 {
			if len(body.Images) > 9 {
				span.SetStatus(codes.Error, "too many images")
				writeSSE(w, map[string]any{"type": "error", "message": "图片数量过多（最多 9 张）"})
				return
			}
			for _, im := range body.Images {
				if err := validateImage(im); err != nil {
					span.SetStatus(codes.Error, err.Error())
					writeSSE(w, map[string]any{"type": "error", "message": err.Error()})
					return
				}
			}
			mc, merr := sessionSvc.GetDefaultModelConfig(ctx, app)
			if merr != nil {
				span.SetStatus(codes.Error, merr.Error())
				writeSSE(w, map[string]any{"type": "error", "message": "读取模型配置失败: " + merr.Error()})
				return
			}
			if mc != nil && !mc.ImageSupport {
				span.SetStatus(codes.Error, "model has no image support")
				writeSSE(w, map[string]any{"type": "error", "message": "当前模型不支持图片输入，请切换到支持视觉的模型后重试"})
				return
			}
		}

		sessionID := body.SessionID
		if sessionID == "" {
			sessionID = uuid.NewString()
		}
		// 会话事件先于组装发出（扩展后续多轮带 sessionId）；组装失败时错误仍带会话上下文
		writeSSE(w, map[string]any{"type": "session", "sessionId": sessionID})
		if flusher != nil {
			flusher.Flush()
		}

		// 错误归一化所需的模型上下文；取不到时走通用回退层（主循环与恢复轮共用）
		appCtx := errs.ModelContext{}
		if mc, merr := sessionSvc.GetDefaultModelConfig(ctx, app); merr == nil && mc != nil {
			appCtx = errs.ModelContext{Provider: mc.Provider, ModelID: mc.ModelID, DisplayName: mc.DisplayName}
		}

		// 中断恢复（P1）：会话末事件悬挂 tool_calls（上一轮在工具调用阶段被停止/超时
		// 打断）→ 先跑恢复轮确定性补执行并续答，再按用户输入决定是否继续正常轮。
		// 交互对齐 Trae：无按钮、无协议字段，用户只管说话；返回 true 表示本轮已终结。
		if resumeIfDangling(ctx, r, sessionSvc, app, user, sessionID, body.Question, reg, appCtx, span, w, flusher) {
			return
		}

		// 组装 runner 入参（scope 继承/参考材料/页面缓存/提示词/工具状态/工作集 L1，
		// 与评估链路共用），仅工作集 L1 超预算返回错误终止本轮。传 span 贯通的 ctx：
		// 组装与后续 run 共用同一 trace，框架 gen_ai.* span 挂到本 span 之下。
		body.SessionID = sessionID
		prep, perr := PrepareInvocation(ctx, app, user, body, InvocationDeps{
			SessionSvc: sessionSvc,
			PS:         ps,
			WSFn:       wsFn,
		})
		if perr != nil {
			span.SetStatus(codes.Error, perr.Error())
			writeSSE(w, map[string]any{"type": "error", "message": perr.Error()})
			return
		}
		requestID := prep.RequestID
		span.SetAttributes(
			attribute.String("request_id", requestID),
			attribute.String("session_id", prep.SessionID),
		)
		reg.set(prep.SessionID, requestID)
		defer reg.del(prep.SessionID)

		eventChan, err := r.Run(prep.RunCtx, user, prep.SessionID, prep.Message, prep.RunOpts...)
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
			writeSSE(w, map[string]any{"type": "error", "message": "agent 启动失败: " + err.Error()})
			return
		}

		// ask_user 挂起：模型调 ask_user 即本轮语义终点。drain 检测到 pending 工具
		// 结果 → 下发卡片帧 → 此后的模型输出（收尾 ack）静默丢弃 → run 自然跑完后
		// 发 done(pending)。不在此处 Cancel：工具结果事件尚可能未落库，立即取消会
		// 产生悬挂 tool_calls，该会话后续所有消息都会触发模型端 400。
		// 续答只能经 /api/ask/answer 携带用户回答以官方 HITL 更新消息发起
		// （design/ask-user-tool-plan.md）。

		// 收到最终响应后发 done，但不 break——继续 drain channel，直到 runner 把
		// 完成事件 append 到 SQLite 并关闭 channel 才退出。这样 req.Context() 在
		// append 完成事件期间不会被取消，避免
		// "Failed to append runner completion event to session: context canceled"。
		sentDone := false
		sentError := false    // 是否已因异常 finish_reason 下发错误提示（防止重复下发）
		var answerText string // 最终回答全文（M3.1 关联检索用，取自 assistant 完整 message）
		var pendingAsk *askuser.PendingOutput
		for evt := range eventChan {
			if evt.Error != nil {
				// 提问挂起后的收尾期异常：不外显（ack 阶段出错无用户可见损失）
				if pendingAsk != nil {
					continue
				}
				norm := errs.FromModel(evt.Error, appCtx)
				span.SetStatus(codes.Error, norm.Message)
				if norm.Code != "" {
					span.SetAttributes(attribute.String("error.code", string(norm.Code)))
				}
				// 技术日志（含原始厂商码/消息）只在此落一次；友好文案经 SSE 下发。
				rawCode := ""
				if evt.Error.Code != nil {
					rawCode = *evt.Error.Code
				}
				log.Errorf("[Absorb] model error | provider=%s code=%s session=%s raw_code=%v raw_msg=%q",
					appCtx.Provider, norm.Code, sessionID, rawCode, evt.Error.Message)
				writeSSE(w, map[string]any{"type": "error", "code": string(norm.Code), "message": norm.Message})
				break
			}
			if evt.Response != nil && len(evt.Response.Choices) > 0 {
				c := evt.Response.Choices[0]
				// ask_user pending 检测：工具结果为 pending JSON 即提问挂起点
				if pendingAsk == nil && c.Message.Role == model.RoleTool && c.Message.Content != "" {
					if p := askuser.ParsePending(c.Message.Content); p != nil {
						pendingAsk = p
						span.SetAttributes(
							attribute.String("ask_user_ticket", p.TicketID),
							attribute.Int("ask_user_options", len(p.Options)),
						)
						log.Infof("[Absorb] ask_user pending (session=%s ticket=%s)", sessionID, p.TicketID)
						writeSSE(w, map[string]any{"type": "ask_user", "ticketId": p.TicketID, "question": p.Question, "options": p.Options, "requestId": requestID})
						if flusher != nil {
							flusher.Flush()
						}
						continue
					}
				}
				if pendingAsk != nil {
					// 提问挂起后模型可能仍在生成收尾内容：不出卡片不外显，静默丢弃
					continue
				}
				// 流式终止原因拦截：content_filter/length 等异常在 HTTP 200 流里由 finish_reason
				// 承载，框架既不报错、也不进 evt.Error，必须在此显式检查。复用 FromModel 同款
				// SSE 形状，前端无需改动即可渲染错误提示。正常停止(stop/tool_calls)与未知值返回 nil。
				if fr := finishReason(c.FinishReason); fr != "" {
					if norm := errs.FromFinishReason(fr); norm != nil && !sentError {
						log.Errorf("[Absorb] stream finish_reason=%s | provider=%s model=%s session=%s code=%s",
							fr, appCtx.Provider, appCtx.ModelID, sessionID, norm.Code)
						writeSSE(w, map[string]any{"type": "error", "code": string(norm.Code), "message": norm.Message})
						if flusher != nil {
							flusher.Flush()
						}
						sentError = true
					}
				}
				// 思考过程流式推送:与 token 互斥(思考阶段 ReasoningContent 有值/Content 空;回答阶段反之)
				if rc := c.Delta.ReasoningContent; rc != "" {
					writeSSE(w, map[string]any{"type": "reasoning", "content": rc})
					if flusher != nil {
						flusher.Flush()
					}
				}
				content := c.Delta.Content
				if content != "" {
					writeSSE(w, map[string]any{"type": "token", "content": content})
					if flusher != nil {
						flusher.Flush()
					}
				}
			}
			if pendingAsk == nil && !sentDone && evt.IsFinalResponse() {
				// 已因异常 finish_reason 下发错误提示时不再发 done；仍继续 drain，
				// 让 runner 完成事件落库。
				if sentError {
					sentDone = true
					continue
				}
				// 记录 LLM 完整回答（assistant message event 的 Choices[0].Message.Content 是合并后的全文）
				if evt.Response != nil && len(evt.Response.Choices) > 0 {
					if c := evt.Response.Choices[0].Message.Content; c != "" {
						answerText = c
						log.Debugf("[Absorb] [%s] LLM answer:\n%s\n---", ps.ID(), c)
					}
				}
				// 记录 token 用量（assistant 完整 message event 携带 Usage，stream_options.include_usage=true 启用）
				if evt.Response != nil {
					if u := evt.Response.Usage; u.TotalTokens > 0 {
						log.Debugf("[Absorb] [%s] token usage: prompt=%d completion=%d reasoning=%d total=%d",
							ps.ID(), u.PromptTokens, u.CompletionTokens,
							u.CompletionTokensDetails.ReasoningTokens, u.TotalTokens)
						// 上报统计（kind=chat）：模型名取请求开始时解析的默认模型配置
						// （与 dynamicModel 同源）；统计是旁路，失败不影响问答主链路。
						tokentracker.Track(tokentracker.KindChat, appCtx.ModelID, tokentracker.Usage{
							PromptTokens:     u.PromptTokens,
							CompletionTokens: u.CompletionTokens,
							ReasoningTokens:  u.CompletionTokensDetails.ReasoningTokens,
							TotalTokens:      u.TotalTokens,
							CachedTokens:     u.PromptTokensDetails.CachedTokens,
						})
					}
				}
				// requestId 随 done 下发：前端保存知识时作为 originEventId 回传
				// （与 sessionId 组成 origin 二元组，服务端据此算幂等键）
				// M3.1 关联提示（低干扰增量）：回答完成后用规则关键词检索个人知识，
				// 有命中才在 done 前下发 associations（前端流内同步拿到，无需续读）；
				// 失败/零命中均静默——关联绝不影响问答主链路，也不强行制造联系。
				if ks != nil {
					aq := associationQuery(body, answerText)
					emitAssociationsIfAny(ctx, ks, app, user, aq, requestID, w, flusher)
				}
				writeSSE(w, map[string]any{"done": true, "requestId": requestID})
				if flusher != nil {
					flusher.Flush()
				}
				sentDone = true
				// 不 break：继续 drain 到 runner 关闭 channel，确保完成事件落库
			}
		}
		if pendingAsk != nil && !sentDone {
			// 提问挂起收尾：done 带 pending 标记，前端据此渲染回答卡片
			writeSSE(w, map[string]any{"done": true, "requestId": requestID, "pending": true})
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

// finishReason 安全取出 *string 类型的 finish_reason（nil 视为空串）。
func finishReason(fr *string) string {
	if fr == nil {
		return ""
	}
	return *fr
}

// assocAnswerRunes 关联检索 query 中回答全文的截断长度（rune）：
// 只取摘要开头核心论点，避免长回答稀释关键词。
const assocAnswerRunes = 300

// assocPlaceholderRe 引用占位符 [[n]]：问题里的 chip 占位对检索是噪声，剥掉。
var assocPlaceholderRe = regexp.MustCompile(`\[\[\d+\]\]`)

// truncateRunes rune 安全截断（associationQuery 的回答摘录用）。
func truncateRunes(s string, n int) string {
	rs := []rune(strings.TrimSpace(s))
	if len(rs) <= n {
		return string(rs)
	}
	return string(rs[:n])
}

// associationQuery 从问答请求 + 最终回答构造 M3.1 检索上下文。
// kind 判定：带划词上下文（Context.Text 非空）→ selection，否则 summary；
// selection 的检索输入由关键词源按 SelectedText → UserQuestion 优先级取词，
// summary 用「问题 + 回答核心开头」；页面标题始终作为兜底补充。
func associationQuery(in InvocationInput, answer string) knowledge.RetrievalQuery {
	question := strings.TrimSpace(assocPlaceholderRe.ReplaceAllString(in.Question, " "))
	q := knowledge.RetrievalQuery{Kind: knowledge.RetrievalKindSummary, Query: question}
	if in.Context != nil && strings.TrimSpace(in.Context.Text) != "" {
		q.Kind = knowledge.RetrievalKindSelection
		q.SelectedText = in.Context.Text
		q.UserQuestion = question
	} else if answer != "" {
		q.Query = question + "\n" + truncateRunes(answer, assocAnswerRunes)
	}
	if in.Page != nil {
		q.PageTitle = in.Page.Title
	}
	return q
}

// emitAssociationsIfAny 执行 M3.1 关联检索并按需下发 associations 事件：
// 检索子 span（挂 ask 根下）记录关键词与命中规模，零命中也可观测；
// 检索失败仅告警静默，命中才写 SSE。
func emitAssociationsIfAny(ctx context.Context, ks *knowledge.Service, app, user string,
	aq knowledge.RetrievalQuery, requestID string, w http.ResponseWriter, flusher http.Flusher) {
	rctx, rspan := otel.Tracer("absorb/chat").Start(ctx, "retrieval.associations")
	cands, kws, serr := ks.SearchRelevant(rctx, app, user, aq, knowledge.RetrievalOptions{})
	if serr != nil {
		rspan.SetStatus(codes.Error, serr.Error())
		rspan.End()
		log.Warnf("[Absorb] knowledge association search failed: %v", serr)
		return
	}
	rspan.SetAttributes(
		attribute.StringSlice("keywords", kws),
		attribute.String("kind", string(aq.Kind)),
		attribute.Int("candidate_count", len(cands)),
	)
	rspan.End()
	if len(cands) > 0 {
		writeSSE(w, map[string]any{
			"type": "associations", "requestId": requestID,
			"kind": string(aq.Kind), "candidates": cands,
		})
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func writeSSE(w http.ResponseWriter, data any) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(w, "data: %s\n\n", b)
}

// writeJSON 写普通 JSON 响应（非 SSE，用于 /api/cancel）
func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

// CancelHandler 处理 /api/cancel：按 sessionId 定位当前 run 并中断。
// 与 SSE 连接生命周期解耦——主动停止不依赖 abort/断连,而是显式取消请求。
// r.Run 已开启 WithDetachedCancel(true),run 不继承 req.Context 取消信号,
// 但 managed.Cancel(requestID) 触发的 run 级 execCancel 仍然生效,可正常停止。
func CancelHandler(r runner.Runner, reg *RunRegistry) http.HandlerFunc {
	managed, ok := r.(runner.ManagedRunner)
	if !ok {
		return func(w http.ResponseWriter, req *http.Request) {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"code": 1, "message": "runner does not support cancel"})
		}
	}
	return func(w http.ResponseWriter, req *http.Request) {
		// CORS 响应头（OPTIONS 预检与方法校验由路由层承担）
		w.Header().Set("Access-Control-Allow-Origin", "*")
		var body struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"code": 1, "message": "请求体解析失败: " + err.Error()})
			return
		}
		requestID := reg.get(body.SessionID)
		if requestID == "" {
			writeJSON(w, http.StatusOK, map[string]any{"code": 0, "canceled": false})
			return
		}
		if !managed.Cancel(requestID) {
			// 命中了 registry 但 Cancel 未找到对应 run（可能刚好结束），按无事发生返回
			reg.del(body.SessionID)
			writeJSON(w, http.StatusOK, map[string]any{"code": 0, "canceled": false})
			return
		}
		log.Infof("[Absorb] run cancel requested (session=%s requestID=%s)", body.SessionID, requestID)
		writeJSON(w, http.StatusOK, map[string]any{"code": 0, "canceled": true})
	}
}
