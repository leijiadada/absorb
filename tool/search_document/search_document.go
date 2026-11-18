// search_document 工具（M1）：PDF/长文档全文证据检索。
// 与 expand_context 分工（doc2 §五）：expand_context 在页面缓存（HTML 网页）
// 上做子串窗口检索；search_document 在 document_chunks + FTS5 上做 BM25 全文
// 检索——30k 缓存截断之外的长文档证据由此可达。
// 当前文档范围由服务端经 ctx 注入（PrepareInvocation 锚定的 documentId），
// 不接受模型指定 document_id——范围与归属校验都在服务端。
package searchdoc

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"trpc.group/trpc-go/trpc-agent-go/tool"
	"trpc.group/trpc-go/trpc-agent-go/tool/function"

	"absorb-agent/internal/document"
	"absorb-agent/internal/log"
	"absorb-agent/internal/retrieval"
	"absorb-agent/internal/store/storage"
)

// Searcher 检索依赖：storage.Storage 的方法子集（测试可替身）。
type Searcher interface {
	SearchDocumentChunks(ctx context.Context, app, user, query string, documentIDs []string, limit int) ([]storage.DocumentChunkHit, error)
}

// VectorSource 语义向量加载（M3）：storage.Storage 的方法子集。
type VectorSource interface {
	LoadVectorHits(ctx context.Context, app, user, model string, documentIDs []string) ([]storage.DocumentChunkHit, []storage.ChunkVectorRow, error)
}

// hitContentRunes 单条命中返回给模型的正文上限：足够理解论据，多条不撑爆输出。
const hitContentRunes = 1200

type searchDocumentInput struct {
	Query string `json:"query"` // 检索问句/关键词组：文档原文可能使用的表述（中英皆可）
	TopK  int    `json:"top_k,omitempty"`
}

type searchDocumentOutput struct {
	Success bool   `json:"success"`
	Content string `json:"content,omitempty"` // 命中片段（按相关度排序，含来源标注）
	Message string `json:"message,omitempty"` // 失败/零命中说明
}

// searchState 单次 /api/ask 请求内的检索上下文。
type searchState struct {
	searcher    Searcher
	app, user   string
	documentIDs []string           // 空 = 当前用户全部已索引文档
	embedder    retrieval.Embedder // M3：nil = 纯词法（配置未启用/失败降级）
	vectors     VectorSource       // M3：与 embedder 同时注入
	calls       atomic.Int32       // 调用计数观测（模型是否反复检索）
}

type searchStateKey struct{}

// WithDocumentSearchState 纯词法检索状态（M2 语义保留）。
func WithDocumentSearchState(ctx context.Context, searcher Searcher, app, user string, documentIDs []string) context.Context {
	return context.WithValue(ctx, searchStateKey{}, &searchState{
		searcher: searcher, app: app, user: user, documentIDs: documentIDs,
	})
}

// WithHybridSearchState 双路检索状态（M3）：embedder/vectors 非空时启用语义路，
// RRF 融合；任一为空自动降级纯词法。当前文档范围仍由服务端注入。
func WithHybridSearchState(ctx context.Context, searcher Searcher, embedder retrieval.Embedder, vectors VectorSource, app, user string, documentIDs []string) context.Context {
	return context.WithValue(ctx, searchStateKey{}, &searchState{
		searcher: searcher, app: app, user: user, documentIDs: documentIDs,
		embedder: embedder, vectors: vectors,
	})
}

func stateFromContext(ctx context.Context) *searchState {
	if v, ok := ctx.Value(searchStateKey{}).(*searchState); ok {
		return v
	}
	return nil
}

// NewSearchDocumentTool 注册 search_document 工具。
func NewSearchDocumentTool() tool.CallableTool {
	fn := func(ctx context.Context, in searchDocumentInput) (searchDocumentOutput, error) {
		st := stateFromContext(ctx)
		if st == nil || st.searcher == nil {
			return searchDocumentOutput{Success: false, Message: "文档检索状态不可用"}, nil
		}
		_, span := otel.Tracer("absorb/searchdoc").Start(ctx, "search_document")
		defer span.End()
		q := strings.TrimSpace(in.Query)
		if q == "" {
			return searchDocumentOutput{Success: false, Message: "query 不能为空"}, nil
		}
		limit := in.TopK
		if limit <= 0 {
			limit = 6
		}
		if limit > 10 {
			limit = 10
		}
		// 查询与索引共用同一分析器（分词不一致即检索失效）
		match := document.SharedAnalyzer().QueryExpression(q)
		if match == "" {
			return searchDocumentOutput{Success: false, Message: "检索词无效（无可索引词项）"}, nil
		}
		started := time.Now()
		// M3 双路召回：词法（FTS5+BM25）+ 语义（余弦 topK），RRF 融合后进
		// M2 多样性管线。语义路失败静默降级——词法路永远完整可用。
		lexical, err := st.searcher.SearchDocumentChunks(ctx, st.app, st.user, match, st.documentIDs, RawRecallLimit)
		callIdx := st.calls.Add(1)
		if err != nil {
			log.Warnf("[Absorb] search_document query failed (q=%q): %v", q, err)
			return searchDocumentOutput{Success: false, Message: "检索失败，请稍后重试或基于已有材料回答"}, nil
		}
		var vectorHits []storage.DocumentChunkHit
		if st.embedder != nil && st.vectors != nil {
			if qv, qerr := st.embedder.EmbedQuery(ctx, q); qerr == nil {
				if vhits, vrows, verr := st.vectors.LoadVectorHits(ctx, st.app, st.user, st.embedder.Model(), st.documentIDs); verr == nil {
					vectorHits = retrieval.TopKByCosine(vhits, vrows, qv, RawRecallLimit)
				} else {
					log.Warnf("[Absorb] search_document vector load failed: %v", verr)
				}
			} else {
				log.Warnf("[Absorb] search_document query embedding failed (lexical-only): %v", qerr)
			}
		}
		fused := retrieval.FuseRRF(lexical, vectorHits, RawRecallLimit)
		// 精排（M3 可选）：SharedReranker 非空时按 F4b 扩池口径重排——池 =
		// 融合榜 topN ∪ 向量种子 top40（autopsy 机制先验，见 design/5.md），
		// 整池重排、池外融合尾原序缀后，失败降级融合序。rerankState 供
		// span/日志观测：off=未配置 / applied=生效 / fallback=调用失败降级
		// （降级时内部已记 Warn 日志）
		rr := retrieval.SharedReranker()
		fused, rerankApplied := retrieval.RerankPoolCandidates(ctx, fused, vectorHits, q, rr, retrieval.SharedRerankTopN(), retrieval.RerankSeedTopN)
		rerankState := "off"
		if rr != nil {
			if rerankApplied {
				rerankState = "applied"
			} else {
				rerankState = "fallback"
			}
		}
		// 范围感知的结果裁剪：单文档范围（当前阅读的资料）只去重 + 总量控制，
		// per-doc 配额会把 top_k 硬性压到 3（Recall@5/10 失真）；跨文档范围才
		// 按 M2 多样性轮转（防长文垄断、短文无曝光）。
		var final []storage.DocumentChunkHit
		var docsCovered int
		if len(st.documentIDs) == 1 {
			final, docsCovered = DeduplicateHits(fused, limit)
		} else {
			final, docsCovered = DiversifyHits(fused, PerDocLimit, limit)
		}
		dur := time.Since(started).Round(time.Millisecond)
		scope := "all_documents"
		if len(st.documentIDs) > 0 {
			scope = "scoped"
		}
		span.SetAttributes(
			attribute.String("query", q),
			attribute.Int("lexical_hits", len(lexical)),
			attribute.Int("vector_hits", len(vectorHits)),
			attribute.Int("final_hits", len(final)),
			attribute.Int("docs_covered", docsCovered),
			attribute.String("scope", scope),
			attribute.String("rerank", rerankState),
			attribute.Int("call_index", int(callIdx)),
		)
		log.Infof("[Absorb] search_document call#%d: q=%q lex=%d vec=%d final=%d docs=%d scope=%s rerank=%s dur=%s",
			callIdx, q, len(lexical), len(vectorHits), len(final), docsCovered, scope, rerankState, dur)
		if len(fused) == 0 {
			return searchDocumentOutput{
				Success: true,
				Content: fmt.Sprintf("（全文检索零命中：%q 在当前文档范围内没有匹配证据，不代表内容不存在——可换用文档原文可能使用的词再检索一次，或基于已有材料回答）", q),
			}, nil
		}
		return searchDocumentOutput{Success: true, Content: renderHits(final)}, nil
	}
	return function.NewFunctionTool(fn,
		function.WithName("search_document"),
		function.WithDescription(
			"检索个人资料库的全文证据（当前文档，或用户已索引的全部资料）。当问题涉及文档后文、具体论据、数据、"+
				"或想跨资料查找相关内容时调用：对全文做关键词+语义混合检索，返回最相关的原文片段（按资料分组、附资料标题）。"+
				"提供 query——不要复制用户原话：先用对话上下文消解指代（\"它 / 这本书 / 刚才提到的\" 换成具体对象名），再把口语转为"+
				"资料中可能出现的书面表述（中英术语皆可）。例：用户问「它的部署方式是什么」且上文在讨论 Docker → query 传 "+
				"\"Docker 部署方式\"，不传 \"它的部署方式\"。"+
				"零命中时返回信号：先换词再试一次，仍无则如实说明资料未覆盖该内容。"+
				"不适用：解释概念本身（直接回答）；获取其他网页（用 fetch_page）；"+
				"恢复当前网页页面内容（用 expand_context）。调用时不要输出正文说明，直接发起调用。",
		),
		function.WithInputSchema(&tool.Schema{
			Type:     "object",
			Required: []string{"query"},
			Properties: map[string]*tool.Schema{
				"query": {
					Type: "string",
					Description: "基于对话上下文消解指代后的检索问句，用资料原文可能出现的书面表述（中英术语皆可），" +
						"完整短语优于单词。例：\"Docker 部署方式\"、\"隧道视野悖论\"",
				},
				"top_k": {
					Type:        "integer",
					Description: "返回片段数上限，默认 6；问概览类问题可给 8-10",
				},
			},
		}),
	)
}

// renderHits 命中渲染（M2 分组版）：按资料聚合，组内列片段——跨文档回答时
// 模型更容易分析「同一份资料内部的连续论据」与「不同资料间的观点差异」。
func renderHits(hits []storage.DocumentChunkHit) string {
	// 按文档分组，保持 bm25 首现顺序（资料编号 = 相关度排名）
	var order []string
	groups := make(map[string][]storage.DocumentChunkHit, len(hits))
	titles := make(map[string]string, len(hits))
	for _, h := range hits {
		if _, ok := groups[h.DocumentID]; !ok {
			order = append(order, h.DocumentID)
		}
		groups[h.DocumentID] = append(groups[h.DocumentID], h)
		titles[h.DocumentID] = h.Title
	}
	var b strings.Builder
	for i, docID := range order {
		hitsOfDoc := groups[docID]
		fmt.Fprintf(&b, "【资料 %d】%s（%d 条片段）\n", i+1, titles[docID], len(hitsOfDoc))
		for _, h := range hitsOfDoc {
			content := []rune(h.Content)
			if len(content) > hitContentRunes {
				content = content[:hitContentRunes]
			}
			if h.SectionPath != "" {
				fmt.Fprintf(&b, "  · %s\n", h.SectionPath)
			}
			b.WriteString("  " + strings.TrimSpace(string(content)) + "\n")
		}
		b.WriteString("\n")
	}
	return b.String()
}
