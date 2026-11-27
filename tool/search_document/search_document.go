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
	"sort"
	"strings"
	"sync"
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
	// 页级阅读模式：start_page > 0 时读取连续原文页，页码来自检索结果标注。
	// query 与 start_page 同时出现按检索处理（互斥规则见工具描述）。
	StartPage int `json:"start_page,omitempty"`
	PageCount int `json:"page_count,omitempty"` // 默认 1，上限 maxReadPages
}

type searchDocumentOutput struct {
	Success bool   `json:"success"`
	Content string `json:"content,omitempty"` // 命中片段（按相关度排序，含来源标注）
	Message string `json:"message,omitempty"` // 失败/零命中说明
}

// ContentReader 文档正文读取依赖（storage.Storage 方法子集）：页号推导与
// 读页模式需要完整正文。
type ContentReader interface {
	LoadDocumentContentByID(ctx context.Context, app, user, docID string) (*storage.DocumentContent, error)
}

// searchState 单次 /api/ask 请求内的检索上下文。
type searchState struct {
	searcher      Searcher
	app, user     string
	documentIDs   []string           // 空 = 当前用户全部已索引文档
	embedder      retrieval.Embedder // M3：nil = 纯词法（配置未启用/失败降级）
	vectors       VectorSource       // M3：与 embedder 同时注入
	contentReader ContentReader      // 页号推导与读页模式；nil = 页能力不可用
	docText       *docTextIndex      // 惰性构建的正文页索引（单请求生命周期）
	mu            sync.Mutex
	calls         atomic.Int32 // 调用计数观测（模型是否反复检索）
}

type searchStateKey struct{}

// WithDocumentSearchState 纯词法检索状态（M2 语义保留）。
func WithDocumentSearchState(ctx context.Context, searcher Searcher, app, user string, documentIDs []string) context.Context {
	return context.WithValue(ctx, searchStateKey{}, &searchState{
		searcher: searcher, app: app, user: user, documentIDs: documentIDs,
	})
}

// WithDocumentQueryState 检索 + 页级阅读统一状态：contentReader 提供完整正文
// 读取（页号推导与读页模式依赖），nil 时页能力不可用、检索行为不变。
func WithDocumentQueryState(ctx context.Context, searcher Searcher, embedder retrieval.Embedder, vectors VectorSource, content ContentReader, app, user string, documentIDs []string) context.Context {
	return context.WithValue(ctx, searchStateKey{}, &searchState{
		searcher: searcher, app: app, user: user, documentIDs: documentIDs,
		embedder: embedder, vectors: vectors, contentReader: content,
	})
}

// WithHybridSearchState 双路检索状态（M3）：embedder/vectors 非空时启用语义路，
// RRF 融合；任一为空自动降级纯词法。当前文档范围仍由服务端注入。
// 页级阅读不可用（无 contentReader），生产链路请用 WithDocumentQueryState。
func WithHybridSearchState(ctx context.Context, searcher Searcher, embedder retrieval.Embedder, vectors VectorSource, app, user string, documentIDs []string) context.Context {
	return WithDocumentQueryState(ctx, searcher, embedder, vectors, nil, app, user, documentIDs)
}

func stateFromContext(ctx context.Context) *searchState {
	if v, ok := ctx.Value(searchStateKey{}).(*searchState); ok {
		return v
	}
	return nil
}

// docTextIndex 单请求内的正文页索引：\f 边界一次扫描，页号查询二分。
type docTextIndex struct {
	text      []rune
	pages     []int // pages[i] = 第 i+1 页起始 rune 偏移
	paginated bool  // 正文含 \f 分页符（主路连续文本为 false）
}

// textIndex 惰性构建正文页索引（单请求内复用）。仅单文档锚定时有意义。
func (s *searchState) textIndex(ctx context.Context) (*docTextIndex, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.docText != nil {
		return s.docText, nil
	}
	if s.contentReader == nil || len(s.documentIDs) != 1 {
		return nil, fmt.Errorf("content reader unavailable")
	}
	dc, err := s.contentReader.LoadDocumentContentByID(ctx, s.app, s.user, s.documentIDs[0])
	if err != nil {
		return nil, err
	}
	if dc == nil {
		return nil, fmt.Errorf("document content not found")
	}
	text := []rune(dc.ContentText)
	pages := []int{0}
	paginated := false
	for i, r := range text {
		if r == '\f' {
			pages = append(pages, i+1)
			paginated = true
		}
	}
	idx := &docTextIndex{text: text, pages: pages, paginated: paginated}
	s.docText = idx
	return idx, nil
}

// pageOf rune 偏移 → 1-based 页号（二分）。
func (d *docTextIndex) pageOf(offset int) int {
	return sort.Search(len(d.pages), func(i int) bool { return d.pages[i] > offset })
}

// pageText 取第 p 页（1-based）文本；越界返回 false。
func (d *docTextIndex) pageText(p int) (string, bool) {
	if p < 1 || p > len(d.pages) {
		return "", false
	}
	start := d.pages[p-1]
	end := len(d.text)
	if p < len(d.pages) {
		end = d.pages[p] - 1 // 掐掉页尾 \f
	}
	return strings.TrimSpace(string(d.text[start:end])), true
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
		// 读页模式：start_page > 0 且无 query（互斥规则见工具描述）
		if in.Query == "" && in.StartPage > 0 {
			out, stats := st.readPages(ctx, in)
			callIdx := st.calls.Add(1)
			span.SetAttributes(
				attribute.String("mode", "read_pages"),
				attribute.Int("page_start", stats.pageStart),
				attribute.Int("pages_returned", stats.pagesReturned),
				attribute.Int("read_runes", stats.readRunes),
				attribute.Int("call_index", int(callIdx)),
			)
			log.Infof("[Absorb] read_pages call#%d: doc=%v start=%d pages=%d runes=%d",
				callIdx, st.documentIDs, stats.pageStart, stats.pagesReturned, stats.readRunes)
			return out, nil
		}
		q := strings.TrimSpace(in.Query)
		if q == "" {
			return searchDocumentOutput{Success: false, Message: "query 不能为空（或传 start_page 进入读页模式）"}, nil
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
		// 精排（F4b 池 + F11 词法种子）：池 = 融合 topN ∪ 向量种子 ∪ 词法种子
		// （默认 A 档 25，rerank_lex_seed 可配 B 档 50；0 视为未配置走默认）。
		// rerankState 供 span/日志观测：off=未配置 / applied=生效 /
		// fallback=调用失败降级（降级时内部已记 Warn 日志）
		rr := retrieval.SharedReranker()
		lexSeed := retrieval.SharedRerankLexSeed()
		fused, rerankApplied := retrieval.RerankPoolCandidates(ctx, fused, vectorHits, lexical, q, rr, retrieval.RerankPoolSpec{
			TopN:    retrieval.SharedRerankTopN(),
			VecSeed: retrieval.RerankSeedTopN,
			LexSeed: lexSeed,
		})
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
		// 页号标注：单文档锚定且正文分页时，hit 偏移按页边界二分映射
		if len(st.documentIDs) == 1 && len(final) > 0 {
			if idx, err := st.textIndex(ctx); err == nil && idx.paginated {
				for i := range final {
					final[i].Page = idx.pageOf(final[i].StartOffset)
				}
			}
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
			attribute.Int("lex_seed", lexSeed),
			attribute.Int("call_index", int(callIdx)),
		)
		log.Infof("[Absorb] search_document call#%d: q=%q lex=%d vec=%d final=%d docs=%d scope=%s rerank=%s lex_seed=%d dur=%s",
			callIdx, q, len(lexical), len(vectorHits), len(final), docsCovered, scope, rerankState, lexSeed, dur)
		if len(fused) == 0 {
			return searchDocumentOutput{
				Success: true,
				Content: fmt.Sprintf("（全文检索零命中：%q 在当前文档范围内没有匹配证据，不代表内容不存在——可换用文档原文可能使用的词再检索一次，或基于已有材料回答）", q),
			}, nil
		}
		content := renderHits(final)
		if in.StartPage > 0 {
			content += "\n（提示：query 与 start_page 同时出现，已按检索处理；读页请单独调用并只传 start_page）"
		}
		return searchDocumentOutput{Success: true, Content: content}, nil
	}
	return function.NewFunctionTool(fn,
		function.WithName("search_document"),
		function.WithDescription(
			"检索并阅读个人资料库文档（当前文档，或用户已索引的全部资料）。两种用法："+
				"1) 检索证据：传 query 做关键词+语义混合检索，返回带章节路径和页码的原文片段，不知道内容在哪时用这个。"+
				"2) 阅读原文：传 start_page 和 page_count（页码来自检索结果的页码标注；page_count 默认 1、最多 3），"+
				"读取连续原文页，需要某段论述的完整上下文时用这个。"+
				"检索的 query 不要复制用户原话：先用对话上下文消解指代（\"它 / 这本书 / 刚才提到的\" 换成具体对象名），再把口语转为"+
				"资料中可能出现的书面表述（中英术语皆可）。例：用户问「它的部署方式是什么」且上文在讨论 Docker → query 传 "+
				"\"Docker 部署方式\"，不传 \"它的部署方式\"。query 与 start_page 二选一，同时出现按检索处理。"+
				"零命中时返回信号：先换词再试一次，仍无则如实说明资料未覆盖该内容。"+
				"不适用：解释概念本身（直接回答）；获取其他网页（用 fetch_page）；"+
				"恢复当前网页页面内容（用 expand_context）。调用时不要输出正文说明，直接发起调用。",
		),
		function.WithInputSchema(&tool.Schema{
			Type:     "object",
			Required: []string{},
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
				"start_page": {
					Type:        "integer",
					Description: "读页模式的起始页号（1-based，页码来自检索结果的页码标注）；与 query 二选一，同时出现按检索处理",
				},
				"page_count": {
					Type:        "integer",
					Description: "读页模式的连续页数，默认 1，最多 3",
				},
			},
		}),
	)
}

// 读页模式常量：单次页数上限与输出预算。
const (
	maxReadPages    = 3
	readBudgetRunes = 6000
)

type readPagesStats struct {
	pageStart     int
	pagesReturned int
	readRunes     int
}

// readPages 页级阅读：按 start_page 读取连续原文页。要求单文档锚定且正文
// 分页（含 \f）；输出按 readBudgetRunes 截断并提示续读方式。
func (st *searchState) readPages(ctx context.Context, in searchDocumentInput) (searchDocumentOutput, readPagesStats) {
	stats := readPagesStats{pageStart: in.StartPage}
	fail := func(msg string) (searchDocumentOutput, readPagesStats) {
		return searchDocumentOutput{Success: false, Message: msg}, stats
	}
	if len(st.documentIDs) != 1 {
		return fail("读页需要先锚定文档：划词或打开文档后提问；也可以先用检索定位内容")
	}
	idx, err := st.textIndex(ctx)
	if err != nil {
		return fail("文档正文不可用，无法按页阅读")
	}
	if !idx.paginated {
		return fail("该文档无分页信息，无法按页阅读，请改用检索")
	}
	count := in.PageCount
	if count <= 0 {
		count = 1
	}
	if count > maxReadPages {
		count = maxReadPages
	}
	var b strings.Builder
	runes := 0
	for p := in.StartPage; p < in.StartPage+count; p++ {
		txt, ok := idx.pageText(p)
		if !ok {
			fmt.Fprintf(&b, "（已到文档末尾，共 %d 页）\n", len(idx.pages))
			break
		}
		rs := []rune(txt)
		remain := readBudgetRunes - runes
		if remain <= 0 {
			fmt.Fprintf(&b, "（输出已达预算，可用 start_page=%d 继续读）\n", p)
			break
		}
		if len(rs) > remain {
			txt = string(rs[:remain]) + "…（本页已截断）"
			rs = rs[:remain]
		}
		fmt.Fprintf(&b, "—— 第 %d 页 ——\n%s\n\n", p, txt)
		stats.pagesReturned++
		runes += len(rs)
		stats.readRunes = runes
		if runes >= readBudgetRunes && p < in.StartPage+count-1 {
			fmt.Fprintf(&b, "（输出已达预算 %d 字，可用 start_page=%d 继续读）\n", readBudgetRunes, p+1)
			break
		}
	}
	return searchDocumentOutput{Success: true, Content: b.String()}, stats
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
				fmt.Fprintf(&b, "  · %s", h.SectionPath)
				if h.Page > 0 {
					fmt.Fprintf(&b, "（p%d）", h.Page)
				}
				fmt.Fprint(&b, "\n")
			} else if h.Page > 0 {
				fmt.Fprintf(&b, "  · p%d\n", h.Page)
			}
			b.WriteString("  " + strings.TrimSpace(string(content)) + "\n")
		}
		b.WriteString("\n")
	}
	return b.String()
}
