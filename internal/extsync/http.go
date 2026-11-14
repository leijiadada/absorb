package extsync

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"absorb-agent/internal/store/storage"
)

// HTTP 扩展端会话镜像同步端点（design/ext-session-sync-design.md）：
// 扩展原生会话对象 JSON 的全量幂等镜像落库（PUT）与恢复读取（索引/详情），
// 桌面端 sessions.db 为权威源，扩展端 chrome.storage 为工作集缓存。
// 路由注册经 Register 注入宿主 mux（bridge.Deps.Extra / CLI mux 共用同一实现）。
type HTTP struct {
	st   storage.Storage
	app  string // 会话归属维度（与问答链路同值）
	user string
}

// NewHTTP 构造端点集。
func NewHTTP(st storage.Storage, app, user string) *HTTP {
	return &HTTP{st: st, app: app, user: user}
}

// maxMirrorBytes 单会话镜像上限：messages 图片本为 192px 缩略图、pageMd 不落
// storage，常态数十 KB；2MB 为防御上限，超限拒绝整条写入。
const maxMirrorBytes = 2 << 20

// titleMaxRunes 列表标题截断长度（首条用户提问）。
const titleMaxRunes = 60

// Register 注册镜像端点（Go 1.22 method-aware 模式，方法不符由 ServeMux
// 自动 405 带 Allow 头）。PUT 跨域需预检；GET 为简单请求响应自带 CORS 头。
func (h *HTTP) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/ext/sessions", h.list)
	mux.HandleFunc("GET /api/ext/sessions/{id}", h.get)
	mux.HandleFunc("PUT /api/ext/sessions/{id}/mirror", h.put)
	mux.HandleFunc("OPTIONS /api/ext/sessions/{id}/mirror", corsPreflight)
}

// corsPreflight 镜像端点跨域预检。
func corsPreflight(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Access-Control-Allow-Methods", "GET, PUT, OPTIONS")
	w.WriteHeader(http.StatusNoContent)
}

// mirrorView 请求体的结构化视图：仅取派生字段（title/msg_count 派生源与时间戳），
// 全文以原始 JSON 原样落库，未知字段（quotes/page/images 等）零丢失。
type mirrorView struct {
	CreatedAt int64 `json:"createdAt"`
	UpdatedAt int64 `json:"updatedAt"`
	Messages  []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

// put PUT /api/ext/sessions/{id}/mirror：全量幂等 upsert。
// sessionId 以路径为权威（body 内同名字段不参与归属判定）；createdAt/updatedAt
// 由扩展端提供（归属事实源），缺失或非法时回退当前时间。
func (h *HTTP) put(w http.ResponseWriter, req *http.Request) {
	setCORS(w)
	sessionID := req.PathValue("id")
	if sessionID == "" {
		writeErr(w, http.StatusBadRequest, "会话 ID 缺失")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxMirrorBytes))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "镜像体积超限（上限 2MB）或请求体读取失败")
		return
	}
	if !json.Valid(body) {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	var view mirrorView
	if err := json.Unmarshal(body, &view); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	err = h.st.UpsertExtMirror(req.Context(), h.app, h.user, sessionID,
		deriveTitle(view.Messages), len(view.Messages), string(body),
		tsOrNow(view.CreatedAt), tsOrNow(view.UpdatedAt))
	if err != nil {
		writeInternalErr(w, "镜像写入失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// list GET /api/ext/sessions：全量索引（updated_at 倒序），不含会话正文。
func (h *HTTP) list(w http.ResponseWriter, req *http.Request) {
	setCORS(w)
	items, err := h.st.ListExtMirrors(req.Context(), h.app, h.user)
	if err != nil {
		writeInternalErr(w, "镜像索引读取失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// get GET /api/ext/sessions/{id}：单会话原生对象（session 字段为镜像 JSON 原文）。
func (h *HTTP) get(w http.ResponseWriter, req *http.Request) {
	setCORS(w)
	rec, err := h.st.GetExtMirror(req.Context(), h.app, h.user, req.PathValue("id"))
	if err != nil {
		writeInternalErr(w, "镜像读取失败", err)
		return
	}
	if rec == nil {
		writeErr(w, http.StatusNotFound, "会话不存在")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sessionId": rec.SessionID,
		"title":     rec.Title,
		"msgCount":  rec.MsgCount,
		"session":   json.RawMessage(rec.Data),
		"createdAt": rec.CreatedAt,
		"updatedAt": rec.UpdatedAt,
	})
}

// deriveTitle 取首条用户提问截断为列表标题；纯图片等无文本轮次返回空串，
// 由展示层回退到时间占位。
func deriveTitle(msgs []struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}) string {
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		t := strings.TrimSpace(m.Content)
		if t == "" {
			continue
		}
		r := []rune(t)
		if len(r) > titleMaxRunes {
			return string(r[:titleMaxRunes]) + "…"
		}
		return t
	}
	return ""
}

// tsOrNow 扩展端毫秒时间戳 → Unix 秒（>1e12 视为毫秒）；缺失/非法回退当前时间。
func tsOrNow(v int64) time.Time {
	if v > 1_000_000_000_000 {
		v /= 1000
	}
	if v <= 0 {
		return time.Now()
	}
	return time.Unix(v, 0)
}

// setCORS 跨域响应头（GET 简单请求浏览器要求响应带头才可读）。
func setCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
}

// writeErr 错误响应体与知识端点同形状（{"code":1,"message":...}）。
func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"code": 1, "message": msg})
}

// writeInternalErr 服务端错误统一 500：细节进日志，响应给固定文案。
func writeInternalErr(w http.ResponseWriter, msg string, err error) {
	writeErr(w, http.StatusInternalServerError, msg+": "+err.Error())
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}
