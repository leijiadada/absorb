package knowledge

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"absorb-agent/internal/log"
)

// HTTP 知识持久化 HTTP 端点（M1 第三步）：把 Service 暴露给前端（扩展侧栏 / 桌面 GUI），
// 并把 Service 变更事件以 SSE 广播。路由注册经 Register 注入宿主 mux
// （bridge.Deps.Extra / CLI mux 共用同一实现）。
type HTTP struct {
	svc     *Service
	app     string // 会话归属维度（与问答链路同值，M1 仅做数据隔离）
	user    string
	hub     *hub
	draftFn DraftFunc // 草稿 LLM 实现（M2，SetDraft 注入；nil=端点走降级路径）
}

// NewHTTP 构造端点集，并把 Service 变更事件接到内部广播器（进程内 fan-out，非阻塞）。
func NewHTTP(svc *Service, app, user string) *HTTP {
	h := &HTTP{svc: svc, app: app, user: user, hub: newHub()}
	svc.SetOnChange(h.hub.publish)
	return h
}

// Register 注册全部知识端点（由宿主在组装路由时调用；Go 1.22 method-aware 模式，
// 方法不符由 ServeMux 自动 405 带 Allow 头，处理器内不再手工校验 req.Method）。
func (h *HTTP) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/knowledge", h.create)
	mux.HandleFunc("POST /api/knowledge/draft", h.draft)       // M2：保存面板预填草稿（字面段，不与 {id} 冲突）
	mux.HandleFunc("POST /api/reading-summary", h.saveSummary) // M2.5：保存阅读摘要（层次A，不建知识）
	mux.HandleFunc("GET /api/knowledge", h.list)
	mux.HandleFunc("GET /api/knowledge/events", h.events) // 字面段优先于 {id}，不冲突
	mux.HandleFunc("GET /api/knowledge/{id}", h.get)
	mux.HandleFunc("PATCH /api/knowledge/{id}", h.patch)
	mux.HandleFunc("DELETE /api/knowledge/{id}", h.del)
	// 写类端点（POST/PATCH/DELETE）跨域需预检；GET 为简单请求无需预检。
	// 预检按路径注册，同一 {id} 路径同时放行 PATCH 与 DELETE。
	mux.HandleFunc("OPTIONS /api/knowledge", corsPreflight)
	mux.HandleFunc("OPTIONS /api/knowledge/draft", corsPreflight)
	mux.HandleFunc("OPTIONS /api/knowledge/{id}", corsPreflight)
	mux.HandleFunc("OPTIONS /api/reading-summary", corsPreflight)
}

// corsPreflight 知识端点跨域预检：方法集覆盖本组全部动词（区别于 bridge.CorsPreflight 的 POST-only）。
func corsPreflight(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
	w.WriteHeader(http.StatusNoContent)
}

// ---------- 端点 ----------

// create POST /api/knowledge：保存知识（幂等）。origin 二元组由前端从问答链路
// 携带（session 事件的 sessionId + done 事件的 requestId），服务端据此计算
// dedup_key，防伪造绕过幂等。新建返回 201，幂等命中返回 200（created=false）。
func (h *HTTP) create(w http.ResponseWriter, req *http.Request) {
	setCORS(w)
	var body struct {
		Title             string        `json:"title"`
		AISummary         string        `json:"aiSummary"`
		UserUnderstanding string        `json:"userUnderstanding"`
		OriginSessionID   string        `json:"originSessionId"`
		OriginEventID     string        `json:"originEventId"`
		Sources           []SourceInput `json:"sources"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	// 保存动作 span：记录来源规模与创建/幂等结果（吸收行为观测的数据源）
	ctx, span := otel.Tracer("absorb/knowledge").Start(req.Context(), "knowledge.save")
	defer span.End()
	item, created, err := h.svc.Create(ctx, h.app, h.user, CreateInput{
		Title:             body.Title,
		AISummary:         body.AISummary,
		UserUnderstanding: body.UserUnderstanding,
		OriginSessionID:   body.OriginSessionID,
		OriginEventID:     body.OriginEventID,
		Sources:           body.Sources,
	})
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		writeServiceErr(w, err)
		return
	}
	span.SetAttributes(
		attribute.Int("title_len", len([]rune(body.Title))),
		attribute.Int("source_count", len(body.Sources)),
		attribute.Bool("created", created),
	)
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"created": created, "item": item})
}

// saveSummary POST /api/reading-summary：保存阅读摘要（M2.5 层次A）。
// 轻操作：不弹编辑面板，前端从回答事件携带全文直接保存；幂等（同一回答
// 重复提交 created=false）。保存摘要永不创建 Knowledge Item——总结与吸收
// 是两类行为（设计文档 §一）。
func (h *HTTP) saveSummary(w http.ResponseWriter, req *http.Request) {
	setCORS(w)
	var body struct {
		SessionID   string `json:"sessionId"`
		RequestID   string `json:"requestId"`
		URL         string `json:"url"`
		Title       string `json:"title"`
		SummaryText string `json:"summaryText"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	doc, created, err := h.svc.SaveReadingSummary(req.Context(), h.app, h.user, SaveSummaryInput{
		SessionID:   body.SessionID,
		RequestID:   body.RequestID,
		URL:         body.URL,
		Title:       body.Title,
		SummaryText: body.SummaryText,
	})
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"created": created, "documentId": doc.ID})
}

// list GET /api/knowledge?status=：知识列表（空=全部，非法值 400）。
func (h *HTTP) list(w http.ResponseWriter, req *http.Request) {
	setCORS(w)
	items, err := h.svc.List(req.Context(), h.app, h.user, req.URL.Query().Get("status"))
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// get GET /api/knowledge/{id}：条目详情（含来源，供"查看原始来源"）。
func (h *HTTP) get(w http.ResponseWriter, req *http.Request) {
	setCORS(w)
	item, sources, err := h.svc.Get(req.Context(), h.app, h.user, req.PathValue("id"))
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"item": item, "sources": sources})
}

// patch PATCH /api/knowledge/{id}：部分更新。文本三字段缺省保留原值（合并语义，
// Service.Update 为整体覆盖，故先取既有值补齐）；status 缺省不动，提供则走归档/重开。
// 文本与 status 可同时提交（顺序：先文本后状态，各发一次 updated 事件）。
func (h *HTTP) patch(w http.ResponseWriter, req *http.Request) {
	setCORS(w)
	var body struct {
		Title             *string `json:"title"`
		AISummary         *string `json:"aiSummary"`
		UserUnderstanding *string `json:"userUnderstanding"`
		Status            *string `json:"status"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	id := req.PathValue("id")
	if body.Title != nil || body.AISummary != nil || body.UserUnderstanding != nil {
		item, _, err := h.svc.Get(req.Context(), h.app, h.user, id)
		if err != nil {
			writeServiceErr(w, err)
			return
		}
		title, summary, understanding := item.Title, item.AISummary, item.UserUnderstanding
		if body.Title != nil {
			title = *body.Title
		}
		if body.AISummary != nil {
			summary = *body.AISummary
		}
		if body.UserUnderstanding != nil {
			understanding = *body.UserUnderstanding
		}
		if err := h.svc.Update(req.Context(), h.app, h.user, id, title, summary, understanding); err != nil {
			writeServiceErr(w, err)
			return
		}
	}
	if body.Status != nil {
		if err := h.svc.SetStatus(req.Context(), h.app, h.user, id, *body.Status); err != nil {
			writeServiceErr(w, err)
			return
		}
	}
	item, _, err := h.svc.Get(req.Context(), h.app, h.user, id)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"item": item})
}

// del DELETE /api/knowledge/{id}：硬删除（目标不存在幂等 204）。
func (h *HTTP) del(w http.ResponseWriter, req *http.Request) {
	setCORS(w)
	if err := h.svc.Delete(req.Context(), h.app, h.user, req.PathValue("id")); err != nil {
		writeServiceErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// events GET /api/knowledge/events：知识变更 SSE 流。data-only 帧（与问答流同编码约定），
// type ∈ knowledge.created/updated/deleted，item 为变更后条目快照。连接生命周期内
// 持续推送；断连由 req.Context 取消感知并退订。
func (h *HTTP) events(w http.ResponseWriter, req *http.Request) {
	setCORS(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	ch := h.hub.subscribe()
	defer h.hub.unsubscribe(ch)
	flusher.Flush()
	for {
		select {
		case <-req.Context().Done():
			return
		case frame := <-ch:
			if _, err := w.Write(frame); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// ---------- 广播器 ----------

// hub 进程内事件 fan-out：Service 变更回调 → publish → 各 SSE 订阅者。
// 发布为非阻塞：订阅者积压超过缓冲即丢弃并告警（前端下次拉列表自愈，宁丢勿堵）。
type hub struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

func newHub() *hub {
	return &hub{subs: make(map[chan []byte]struct{})}
}

func (h *hub) subscribe() chan []byte {
	ch := make(chan []byte, 16)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *hub) unsubscribe(ch chan []byte) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

// publish 序列化一次、以 SSE data 帧广播给全部订阅者。
func (h *hub) publish(ev KnowledgeEvent) {
	payload, err := json.Marshal(ev)
	if err != nil {
		log.Warnf("[Absorb] knowledge event marshal failed: %v", err)
		return
	}
	frame := append([]byte("data: "), payload...)
	frame = append(frame, '\n', '\n')
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- frame:
		default:
			log.Warnf("[Absorb] knowledge event dropped (subscriber buffer full)")
		}
	}
}

// ---------- 通用 ----------

// setCORS 跨域响应头（GET 简单请求浏览器要求响应带头才可读）。
func setCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
}

// writeErr 错误响应体与 /api/cancel 同形状（{"code":1,"message":...}）。
func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"code": 1, "message": msg})
}

// writeServiceErr 服务层错误 → HTTP 状态映射：来源会话缺失 400（语义区分），
// 不存在 404，入参校验 400，其余 500。
func writeServiceErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrOriginSession):
		// ErrOriginSession 包装了 ErrValidation，须先判——语义是"来源缺依据"而非字段缺失
		writeErr(w, http.StatusBadRequest, "来源会话不存在或已清理，无法保存。")
	case errors.Is(err, ErrNotFound):
		writeErr(w, http.StatusNotFound, "知识不存在")
	case errors.Is(err, ErrValidation):
		// 服务层校验错误细节是英文实现文案，不透给用户；给固定中文指引
		writeErr(w, http.StatusBadRequest, "保存内容校验未通过：标题必填，且来源信息不完整。")
	default:
		log.Errorf("[Absorb] knowledge service error: %v", err)
		writeErr(w, http.StatusInternalServerError, "知识操作失败: "+err.Error())
	}
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}
