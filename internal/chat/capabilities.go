package chat

import (
	"net/http"

	"absorb-agent/internal/log"
	"absorb-agent/internal/store"
)

// CapabilitiesHandler 处理 GET /api/capabilities：向扩展端下发当前默认模型的能力标志。
// 软闸数据源——扩展端 hasVisionModel() 依赖 settings.imageSupport，该字段此前无任何
// 写入方（本地设置页与后端下发均无），软闸恒开；经此端点下发后扩展端入口禁用与
// 1210 提示回写才可闭环（见 design/image-pipeline-plan.md 方案 B1）。
// 语义与 AskHandler 前置闸门一致：无模型配置（mc==nil）默认 imageSupport=true，
// 读取失败降级为 true——硬拦截兜底在 /api/ask 侧，软闸只影响扩展端入口显隐。
func CapabilitiesHandler(sessionSvc *store.SessionService, app string) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		// CORS 响应头（GET 简单请求无预检，同 /health）
		w.Header().Set("Access-Control-Allow-Origin", "*")

		imageSupport := true
		mc, err := sessionSvc.GetDefaultModelConfig(req.Context(), app)
		if err != nil {
			log.Warnf("[Absorb] capabilities: read model config failed, degrade to imageSupport=true: %v", err)
		} else if mc != nil {
			imageSupport = mc.ImageSupport
		}
		writeJSON(w, http.StatusOK, map[string]any{"imageSupport": imageSupport})
	}
}
