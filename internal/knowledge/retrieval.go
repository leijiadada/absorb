package knowledge

import (
	"context"
	"sort"
	"strings"
	"unicode"

	"absorb-agent/internal/log"
	"absorb-agent/internal/store/storage"
)

// M3.1 相关知识检索——接口契约（评测先行：本文件先于实现落地）。
//
// 设计文档：design/m3.1-retrieval-service-design.md；
// 评测集：design/eval/knowledge-recall/（corpus.yaml / cases.yaml），
// harness 面向本文件的 RetrievalSearcher 契约运行，fake/真实实现可替换。
//
// 关键决策（已评审确认）：
//   - 打分权重：title=3 / user_understanding=3 / ai_summary=2 / quote_text=1 / source_title=1；
//     MinScore 默认 2，Limit 默认 5（实现步落地为常量，评测驱动调优）。
//   - 关键词生成：M3.1 纯规则，不接 LLM；KeywordSource 是替换 seam，
//     观测（关键词、命中归因日志）作为引入 LLM 关键词源的决策依据。

// RetrievalKind 触发场景（与 M3.1 设计 §3/§6 对应）：
//   - summary：用户要求总结全文；摘要仍是第一交付物，检索在摘要生成后进行
//   - selection：划词追问；SelectedText + UserQuestion 最精确
//   - compare：用户明确要求比较；检索是本轮任务的一部分，直接执行
type RetrievalKind string

const (
	RetrievalKindSummary   RetrievalKind = "summary"
	RetrievalKindSelection RetrievalKind = "selection"
	RetrievalKindCompare   RetrievalKind = "compare"
)

// Valid 判断 kind 是否为受支持的触发场景。
func (k RetrievalKind) Valid() bool {
	switch k {
	case RetrievalKindSummary, RetrievalKindSelection, RetrievalKindCompare:
		return true
	}
	return false
}

// RetrievalQuery 当前阅读上下文。字段按场景部分填写：
// selection 必有 SelectedText/UserQuestion；summary 的 Query 为摘要核心论点；
// compare 的 Query 为用户的比较请求。均不要求携带整篇原文。
type RetrievalQuery struct {
	Kind         RetrievalKind
	Query        string // 主检索文本（摘要核心论点 / 比较请求）
	PageTitle    string
	SelectedText string // selection 场景：用户划选的原文
	UserQuestion string // selection 场景：用户对划选内容的追问
}

// RetrievalCandidate 单条召回候选。
//
// 观点归属约定：UserUnderstanding 为空表示用户只保存过原文/AI 提炼、
// 未表达个人观点——展示层只能说"你保存过"，不能说"你曾认为"。
type RetrievalCandidate struct {
	ItemID            string                   `json:"itemId"`
	Title             string                   `json:"title"`
	AISummary         string                   `json:"aiSummary"`
	UserUnderstanding string                   `json:"userUnderstanding"`
	TopSource         *storage.KnowledgeSource `json:"topSource"` // 证据预览（quote 截断由展示层处理）
	Score             int                      `json:"score"`
	Coverage          float64                  `json:"coverage"` // 命中关键词数 / 关键词总数，不参与排序，供归因
	MatchedKeywords   []string                 `json:"matchedKeywords"`
}

// RetrievalOptions 检索参数。零值字段由实现取默认值（Limit=5, MinScore=2）。
type RetrievalOptions struct {
	Limit    int
	MinScore int
}

// KeywordSource 关键词生成源。M3.1 生产路径为纯规则实现；
// LLM 关键词源为同接口替换（升级触发条件见设计文档 §9）。
// 评测与确定性测试注入 FixedKeywordSource，保证结果可复现。
type KeywordSource interface {
	Keywords(ctx context.Context, q RetrievalQuery) ([]string, error)
}

// fixedKeywordSource 固定关键词源：始终返回构造时给定的关键词。
type fixedKeywordSource struct {
	kws []string
}

// NewFixedKeywordSource 构造固定关键词源（评测 harness / 单测用）。
func NewFixedKeywordSource(kws []string) KeywordSource {
	return &fixedKeywordSource{kws: kws}
}

func (f *fixedKeywordSource) Keywords(context.Context, RetrievalQuery) ([]string, error) {
	return f.kws, nil
}

// RetrievalSearcher 检索器契约。knowledge.Service 在实现步满足该接口；
// 评测 harness 与上层调用方（任务 3 接线）面向本接口编程。
//
// 契约要点：
//   - 返回的候选已按 Score 降序、updated_at 降序排列，条数 ≤ opts.Limit；
//   - 空候选列表是合法结果（无可靠关联），调用方应静默，不得强行制造关联；
//   - 第二个返回值为实际使用的关键词（归因与观测用）。
type RetrievalSearcher interface {
	SearchRelevant(ctx context.Context, app, user string, q RetrievalQuery, opts RetrievalOptions) ([]*RetrievalCandidate, []string, error)
}

// ---- 打分常量（已评审确认；评测驱动调优，见设计文档 §5）----

const (
	scoreTitle         = 3 // 语义最浓缩
	scoreUnderstanding = 3 // 用户自己的理解是核心观点载体
	scoreSummary       = 2 // AI 提炼，较完整
	scoreQuote         = 1 // 原文摘录，噪声较多
	scoreSourceTitle   = 1 // 来源标题

	defaultLimit    = 5 // 返回候选上限
	defaultMinScore = 2 // 至少命中一个权重≥2 字段，或两个 quote 级命中
	maxKeywords     = 8 // 关键词上限
)

// SetKeywordSource 注入关键词生成源：评测注入 NewFixedKeywordSource，
// 生产不调用（SearchRelevant 惰性取规则源）。
func (s *Service) SetKeywordSource(ks KeywordSource) { s.ks = ks }

// SearchRelevant 相关知识检索（RetrievalSearcher 实现，M3.1）。
// 流程：关键词生成（注入源或规则源）→ storage LIKE 预筛召回 → 字段权重打分
// → MinScore 过滤 → score desc / updated_at desc 排序 → Limit 截断。
// 空候选是合法结果（无可靠关联），调用方应静默。
func (s *Service) SearchRelevant(ctx context.Context, app, user string, q RetrievalQuery, opts RetrievalOptions) ([]*RetrievalCandidate, []string, error) {
	ks := s.ks
	if ks == nil {
		ks = NewRuleKeywordSource()
	}
	raw, err := ks.Keywords(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	kws := normalizeKeywords(raw)
	if len(kws) == 0 {
		log.Infof("knowledge-retrieval: kind=%s input_len=%d empty_keywords（不检索）",
			q.Kind, len(q.Query)+len(q.SelectedText)+len(q.UserQuestion))
		return nil, kws, nil
	}
	if opts.Limit <= 0 {
		opts.Limit = defaultLimit
	}
	if opts.MinScore <= 0 {
		opts.MinScore = defaultMinScore
	}

	hits, err := s.st.SearchKnowledgeWithSources(ctx, app, user, kws, 0)
	if err != nil {
		return nil, kws, err
	}

	var cands []*RetrievalCandidate
	for _, h := range hits {
		c := scoreHit(h, kws)
		if c == nil || c.Score < opts.MinScore {
			continue
		}
		cands = append(cands, c)
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].Score != cands[j].Score {
			return cands[i].Score > cands[j].Score
		}
		return false // 同分保持存储层 updated_at 降序（hits 已按此排序）
	})
	if len(cands) > opts.Limit {
		cands = cands[:opts.Limit]
	}

	if len(cands) == 0 {
		log.Infof("knowledge-retrieval: kind=%s kws=%q pool=%d empty（无可靠关联，静默）", q.Kind, kws, len(hits))
	} else {
		top := cands[0]
		log.Infof("knowledge-retrieval: kind=%s kws=%q pool=%d top=%s score=%d coverage=%.2f matched=%q",
			q.Kind, kws, len(hits), top.ItemID, top.Score, top.Coverage, top.MatchedKeywords)
	}
	return cands, kws, nil
}

// scoreHit 对单条命中按字段权重打分：每个命中关键词计其最高命中字段权重；
// TopSource 取来源级命中的最优来源（同分优先 quote 证据）。
func scoreHit(h *storage.KnowledgeHit, kws []string) *RetrievalCandidate {
	it := h.Item
	score := 0
	var matched []string
	var topSrc *storage.KnowledgeSource
	topSrcW := 0
	for _, kw := range kws {
		lk := strings.ToLower(kw)
		best := 0
		if containsFold(it.Title, lk) {
			best = scoreTitle
		}
		if containsFold(it.UserUnderstanding, lk) && scoreUnderstanding > best {
			best = scoreUnderstanding
		}
		if containsFold(it.AISummary, lk) && scoreSummary > best {
			best = scoreSummary
		}
		for _, src := range h.Sources {
			sw := 0
			if containsFold(src.QuoteText, lk) {
				sw = scoreQuote
			}
			if containsFold(src.SourceTitle, lk) && scoreSourceTitle > sw {
				sw = scoreSourceTitle
			}
			if sw == 0 {
				continue
			}
			if sw > best {
				best = sw
			}
			if sw > topSrcW || (sw == topSrcW && topSrc != nil &&
				src.SourceType == storage.KnowledgeSourceQuote && topSrc.SourceType != storage.KnowledgeSourceQuote) {
				topSrc, topSrcW = src, sw
			}
		}
		if best > 0 {
			score += best
			matched = append(matched, kw)
		}
	}
	if score == 0 {
		return nil
	}
	return &RetrievalCandidate{
		ItemID:            it.ID,
		Title:             it.Title,
		AISummary:         it.AISummary,
		UserUnderstanding: it.UserUnderstanding,
		TopSource:         topSrc,
		Score:             score,
		Coverage:          float64(len(matched)) / float64(len(kws)),
		MatchedKeywords:   matched,
	}
}

// containsFold 大小写不敏感的子串匹配（关键词与字段均已小写化时退化为 Contains）。
func containsFold(field, lowerKW string) bool {
	return strings.Contains(strings.ToLower(field), lowerKW)
}

// normalizeKeywords 关键词清洗：trim、小写化（拉丁字母）、去空、去重保序、上限截断。
func normalizeKeywords(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, kw := range in {
		kw = strings.ToLower(strings.TrimSpace(kw))
		if kw == "" || seen[kw] {
			continue
		}
		seen[kw] = true
		out = append(out, kw)
	}
	if len(out) > maxKeywords {
		out = out[:maxKeywords]
	}
	return out
}

// ---- 规则关键词源（M3.1 生产路径；LLM 关键词源为同接口替换，见设计文档 §9）----

// ruleStopWords 规则切分停止词（最小表，观测日志与评测语料驱动补充）。
var ruleStopWords = map[string]bool{
	// 中文虚词/高频问句成分
	"的": true, "了": true, "是": true, "在": true, "和": true, "与": true,
	"对": true, "为": true, "有": true, "没": true, "不": true, "就": true,
	"都": true, "也": true, "很": true, "到": true, "说": true, "要": true,
	"会": true, "能": true, "这": true, "那": true, "我": true, "你": true,
	"他": true, "她": true, "它": true, "吗": true, "呢": true, "吧": true,
	"什么": true, "怎么": true, "如何": true, "为什么": true, "还是": true,
	"并且": true, "或者": true, "因为": true, "所以": true, "如果": true,
	"但是": true, "然后": true, "以及": true, "通过": true, "关于": true,
	"一个": true, "这个": true, "那个": true, "还": true, "需": true, "已经": true,
	// 英文虚词
	"the": true, "a": true, "an": true, "of": true, "and": true,
	"or": true, "to": true, "is": true, "are": true, "what": true,
	"how": true, "why": true, "with": true, "for": true,
}

// NewRuleKeywordSource 构造规则关键词源（确定性，无 LLM 依赖）。
func NewRuleKeywordSource() KeywordSource { return ruleKeywordSource{} }

type ruleKeywordSource struct{}

// Keywords 规则切分：按 RetrievalKind 决定输入优先级，多级拼接兜底——
//   - selection：SelectedText → UserQuestion → PageTitle
//   - summary / compare：Query → PageTitle
//
// 切分：标点/空白分词 → 小写化 → 停止词二次切分（中文整句无空格时，
// "什么是检索质量"以停止词为边界切出"检索质量"）→ rune≥2 片段保留
// → 去重保序（normalizeKeywords 截断）。关键词均为输入文本子串，
// 保证生产路径可由题面推导（与评测纪律一致）。
func (ruleKeywordSource) Keywords(_ context.Context, q RetrievalQuery) ([]string, error) {
	var parts []string
	if q.Kind == RetrievalKindSelection {
		parts = []string{q.SelectedText, q.UserQuestion, q.PageTitle}
	} else {
		parts = []string{q.Query, q.PageTitle}
	}
	var b strings.Builder
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			b.WriteByte(' ')
			b.WriteString(s)
		}
	}
	fields := strings.FieldsFunc(b.String(), func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r)
	})
	var toks []string
	for _, f := range fields {
		for _, frag := range splitByStopwords(strings.ToLower(f)) {
			if len([]rune(frag)) >= 2 {
				toks = append(toks, frag)
			}
		}
	}
	return normalizeKeywords(toks), nil
}

// splitByStopwords 以停止词为边界反复切分小写片段，返回去空后的干净碎片。
// 关键词在 normalizeKeywords 统一小写化，故直接在 lowercase 域切分，
// 避免原串与 lowering 后索引错位；前缀停止词剥掉、独立成片的停止词过滤。
func splitByStopwords(field string) []string {
	frags := []string{field}
	for changed := true; changed; {
		changed = false
		var next []string
		for _, f := range frags {
			cut := false
			for sw := range ruleStopWords {
				if sw == "" {
					continue
				}
				if i := strings.Index(f, sw); i >= 0 {
					if i > 0 {
						next = append(next, f[:i])
					}
					next = append(next, f[i+len(sw):])
					cut, changed = true, true
					break
				}
			}
			if !cut {
				next = append(next, f)
			}
		}
		frags = next
	}
	out := frags[:0]
	for _, f := range frags {
		if f != "" && !ruleStopWords[f] {
			out = append(out, f)
		}
	}
	return out
}

// 编译期契约断言：Service 满足检索器契约；规则源满足关键词源契约。
var (
	_ RetrievalSearcher = (*Service)(nil)
	_ KeywordSource     = ruleKeywordSource{}
)
