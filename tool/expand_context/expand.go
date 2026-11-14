package expand

import (
	"context"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"trpc.group/trpc-go/trpc-agent-go/tool"
	"trpc.group/trpc-go/trpc-agent-go/tool/function"

	"absorb-agent/internal/log"
	retriever "absorb-agent/tool/pageretriever"
)

// expand_context 工具：页面证据恢复（Page Context Recovery）。
// 问题需要引用页面其他部分的定义/论据/数据、而这些内容不在模型已见材料中时调用；
// 在缓存的页面结构化文本（划词时一次性抽取，存 SQLite）上做本地定向检索，零浏览器往返。
// 检索语义见 pageretriever 包。定位边界（1.md）：解释概念本身靠模型知识，外部网页走 fetch_page。

type expandContextInput struct {
	Keywords []string `json:"keywords"` // 定向检索词：页面原文可能使用的词（含英文原名）
}

type expandContextOutput struct {
	Success bool   `json:"success"`
	Content string `json:"content,omitempty"` // 检索结果（含来源标注/零命中信号）
	Message string `json:"message,omitempty"` // 失败原因
}

// pageSearchState 单次 /api/ask 请求内的检索状态：缓存全文 + 参考材料。
type pageSearchState struct {
	pageText    string       // 划词时抽取缓存的结构化页面全文（可空：无缓存会话）
	refMaterial string       // 参考材料（去重 + 邻近窗口定位用）
	calls       atomic.Int32 // 本请求内调用计数（call# 观测零命中后是否二次检索）
}

type pageSearchStateKey struct{}

// expandCallStats 单次调用的观测数据（决策依据：零命中率 / 回退率 / 命中量）。
type expandCallStats struct {
	ZeroHit     bool   // 有关键词但全页未命中
	Fallback    string // 非空 = 走了回退路径（"ref_mat_window"）
	ResultRunes int    // 返回内容 rune 数
	CacheRunes  int    // 页面缓存 rune 数（0 = 无缓存）
}

// WithPageSearchState 把单次请求的检索状态装入 context，供 expand_context 工具读取。
// 隐藏未导出的 pageSearchState/pageSearchStateKey，供 handler 注入。
func WithPageSearchState(ctx context.Context, pageText, refMat string) context.Context {
	return context.WithValue(ctx, pageSearchStateKey{}, &pageSearchState{pageText: pageText, refMaterial: refMat})
}

func pageSearchStateFromContext(ctx context.Context) *pageSearchState {
	if v, ok := ctx.Value(pageSearchStateKey{}).(*pageSearchState); ok {
		return v
	}
	return nil
}

// NewExpandContextTool 注册 expand_context 工具。
func NewExpandContextTool() tool.CallableTool {
	fn := func(ctx context.Context, in expandContextInput) (expandContextOutput, error) {
		st := pageSearchStateFromContext(ctx)
		if st == nil {
			return expandContextOutput{Success: false, Message: "检索状态不可用"}, nil
		}
		// 工具 span：挂问答 trace 树下（框架工具调用 span 的子级），零命中/
		// 回退/命中量随 trace 可查——与日志行同源双写，查看器里免翻日志。
		_, span := otel.Tracer("absorb/expand").Start(ctx, "expand_context")
		defer span.End()
		out, stats := runExpand(st.pageText, st.refMaterial, in.Keywords)
		callIdx := st.calls.Add(1)
		span.SetAttributes(
			attribute.StringSlice("keywords", in.Keywords),
			attribute.Bool("zero_hit", stats.ZeroHit),
			attribute.String("fallback", stats.Fallback),
			attribute.Int("result_runes", stats.ResultRunes),
			attribute.Int("cache_runes", stats.CacheRunes),
			attribute.Int("call_index", int(callIdx)),
		)
		// 观测：一行/次调用。call# 递增可观测零命中后是否二次检索；
		// 零命中率与命中量是大纲回退优化的数据门槛（先测频率再决定做不做）。
		log.Infof("[Absorb] expand_context call#%d: kws=%v zero_hit=%v fallback=%q result_runes=%d cache_runes=%d",
			callIdx, in.Keywords, stats.ZeroHit, stats.Fallback, stats.ResultRunes, stats.CacheRunes)
		return out, nil
	}
	return function.NewFunctionTool(fn,
		function.WithName("expand_context"),
		function.WithDescription(
			"恢复当前页面中的原文证据。当问题需要引用页面其他部分的定义、论据或数据，"+
				"而这些内容不在你已看到的材料中时调用：对缓存的页面全文做关键词定向检索，本地完成、零网络开销。"+
				"提供 keywords（页面原文可能使用的词，含英文原名），一次性返回所有命中（命中章节、段落、全文片段），无需多次调用。"+
				"关键词全页未命中时返回零命中信号，表示页面没有展开该内容，直接基于自身知识回答。"+
				"不适用：解释概念本身（直接回答）；获取其他网页（用 fetch_page）。"+
				"调用时不要输出正文说明，直接发起调用。",
		),
		function.WithInputSchema(&tool.Schema{
			Type:     "object",
			Required: []string{"keywords"},
			Properties: map[string]*tool.Schema{
				"keywords": {
					Type:        "array",
					Items:       &tool.Schema{Type: "string"},
					Description: "检索词数组：页面原文可能使用的词（含英文原名），如 [\"Runner\", \"多Agent Runner\"]。定位准确性的关键，务必提供",
				},
			},
		}),
	)
}

// RunExpand 单次调用：按 keywords 收集所有命中内容合并返回；无 keywords 时回退到参考材料邻近窗口。
func RunExpand(pageText, refMat string, keywords []string) expandContextOutput {
	out, _ := runExpand(pageText, refMat, keywords)
	return out
}

// runExpand 执行检索并产出观测数据。与 RunExpand 拆分是为了保持旧签名兼容（测试/外部调用）。
func runExpand(pageText, refMat string, keywords []string) (expandContextOutput, expandCallStats) {
	stats := expandCallStats{CacheRunes: len([]rune(pageText))}
	if pageText == "" {
		return expandContextOutput{
			Success: false,
			Message: "本会话没有缓存的页面内容（划词时未获取到页面），无法扩充。请基于已有材料回答。",
		}, stats
	}
	kws := retriever.TrimStrings(keywords)
	if len(kws) > 0 {
		hit, found := retriever.SearchPageText(pageText, kws, refMat)
		stats.ResultRunes = len([]rune(hit))
		if found {
			return expandContextOutput{Success: true, Content: hit}, stats
		}
		stats.ZeroHit = true
		return expandContextOutput{Success: true, Content: retriever.ZeroHitSignal(kws)}, stats
	}
	// 无关键词：参考材料邻近窗口
	w := retriever.RefMatWindow(pageText, refMat)
	stats.Fallback = "ref_mat_window"
	stats.ResultRunes = len([]rune(w))
	return expandContextOutput{Success: true, Content: w}, stats
}
