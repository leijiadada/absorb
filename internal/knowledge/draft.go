package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"absorb-agent/internal/log"
)

// M2 吸收闭环：保存面板预填草稿（AI 建议标题 + AI 提炼）。
// LLM 调用以 DraftFunc 注入（chat 包提供真实现，见 chat.KnowledgeDraftFn），
// 本包不感知模型细节；draftFn 为 nil 时端点仍注册，直接走降级路径——
// 前端只有一条逻辑（ok=false 兜底），无需探测能力。

// DraftPrompt 草稿生成的输入材料。三字段均可空，由处理器裁剪后传给 DraftFunc。
type DraftPrompt struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
	Quote    string `json:"quote,omitempty"`
}

// DraftResult 草稿输出。Title 为空表示模型认为材料不足以提炼（调用方降级）。
type DraftResult struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
}

// DraftFunc LLM 调用契约：给定材料返回标题与提炼。返回 error 或 Title 为空
// 均由 HTTP 层降级（ok=false + 问题截断兜底标题），DraftFunc 实现无需自行降级。
type DraftFunc func(ctx context.Context, in DraftPrompt) (DraftResult, error)

// 输入裁剪上限：约束 token 用量，防超长回答打爆请求。
const (
	draftMaxQuestion = 500
	draftMaxAnswer   = 4000
	draftMaxQuote    = 2000
	draftMaxTitle    = 24 // 降级兜底标题长度
)

// DraftSystemPrompt 草稿生成系统提示词。输出严格 JSON；材料不足时置空标题
// 交由上层降级（防幻觉：宁可手写，不要编造）。chat 包的 LLM 实现引用同一来源。
const DraftSystemPrompt = `你是阅读吸收助手。根据一次 AI 问答的材料，为用户的个人知识库生成保存预填内容。
只输出严格 JSON，无代码围栏、无任何多余文本：{"title":"...","summary":"..."}
规则：
1. title：不超过 20 字，直白陈述这条知识的核心观点；不用引号、书名号或句号结尾。
2. summary：不超过 120 字，只归纳材料中明确表达的内容，不添加材料之外的主张或补充。
3. 材料不足以提炼出观点时，title 与 summary 均置空字符串，不要猜测。`

// SetDraft 注入草稿 LLM 实现（main.go 装配时调用；nil 时端点走降级）。
func (h *HTTP) SetDraft(fn DraftFunc) *HTTP {
	h.draftFn = fn
	return h
}

// draft POST /api/knowledge/draft：保存面板预填草稿。始终 200——生成失败/
// 超时/材料不足统一降级为 ok=false + 兜底标题（问题截断），降级不是错误，
// 用户仍可手写后保存。仅请求体解析失败返回 400。
func (h *HTTP) draft(w http.ResponseWriter, req *http.Request) {
	setCORS(w)
	var body DraftPrompt
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	in := DraftPrompt{
		Question: clampRunes(body.Question, draftMaxQuestion),
		Answer:   clampRunes(body.Answer, draftMaxAnswer),
		Quote:    clampRunes(body.Quote, draftMaxQuote),
	}
	res, err := DraftResult{}, error(nil)
	if h.draftFn == nil {
		err = errors.New("draft not configured")
	} else {
		res, err = h.draftFn(req.Context(), in)
	}
	if err != nil {
		log.Warnf("[Absorb] knowledge draft degraded: %v", err)
	}
	if err != nil || strings.TrimSpace(res.Title) == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      false,
			"title":   fallbackTitle(in.Question),
			"summary": "",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"title":   strings.TrimSpace(res.Title),
		"summary": strings.TrimSpace(res.Summary),
	})
}

// fallbackTitle 降级兜底标题：取用户问题截断（前 24 字）。
func fallbackTitle(question string) string {
	q := strings.TrimSpace(strings.Join(strings.Fields(question), " "))
	if q == "" {
		return "未命名知识"
	}
	return clampRunes(q, draftMaxTitle)
}

// clampRunes 按 rune 数截断（中文按字算，不按字节）。
func clampRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}
