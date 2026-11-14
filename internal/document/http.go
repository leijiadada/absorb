package document

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"absorb-agent/internal/log"
)

// API 文档准备 HTTP 端点（PDF 解读 PR2）：路由注册经 Register 注入宿主 mux
// （bridge.Deps.Extra / CLI mux 共用同一实现，同 knowledge/extsync 端点组）。
type API struct {
	svc *Service
}

// NewAPI 构建端点集。
func NewAPI(svc *Service) *API { return &API{svc: svc} }

// Register 注册文档准备端点（Go 1.22 method-aware 模式，方法不符由 ServeMux
// 自动 405 带 Allow 头）。
func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/page/prepare", a.prepare)
	mux.HandleFunc("OPTIONS /api/page/prepare", corsPreflight)
}

// corsPreflight 跨域预检（写端点需要；同 bridge.CorsPreflight 的 POST 集合）。
func corsPreflight(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.WriteHeader(http.StatusNoContent)
}

// prepareTimeout 单次准备超时：产品定值 10 分钟——大 PDF 下载 + 文本层提取
// 实测 90s+，极端网络（代理慢速下载、超大文件）需充分余量。独立于 fetch_page
// 的模型侧超时——产品级文档准备允许更久，失败有明确状态可重试。
const prepareTimeout = 10 * time.Minute

// prepare POST /api/page/prepare：获取并提取文档正文，落库后返回状态与元信息
// （不含正文）。分类状态（invalid_url/needs_auth/too_large 等）均以 200 返回
// status 字段——它们是产品语义而非协议错误；仅请求体解析失败 400、内部错误 500。
func (a *API) prepare(w http.ResponseWriter, req *http.Request) {
	setCORS(w)
	var body struct {
		URL   string `json:"url"`
		Title string `json:"title"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体解析失败: " + err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(req.Context(), prepareTimeout)
	defer cancel()
	res, err := a.svc.Prepare(ctx, body.URL, body.Title)
	if err != nil {
		log.Warnf("[Absorb] document prepare internal error | url=%q err=%v", body.URL, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func setCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
