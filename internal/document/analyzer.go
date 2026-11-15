// TextAnalyzer 统一文本分析器（M1）：索引侧与查询侧必须共用同一套
// 规范化 + 分词逻辑——两侧词典或规则不一致，检索即静默失效。
// 职责：NFKC 风格规范化（全角→半角、小写、空白折叠）→ 技术词典补词 →
// GSE 搜索模式切分 → 低价值 token 过滤 → 空格拼接（FTS5 unicode61 索引）。
package document

import (
	"regexp"
	"strings"
	"sync"

	"github.com/go-ego/gse"

	"absorb-agent/internal/document/extract"
	"absorb-agent/internal/log"
)

// TokenizerVersion 分词策略版本：词典或切分规则变化时 +1，
// 配合 document_index_state.tokenizer_version 触发全量索引重建。
// v2：加入 PDF 逐字符空格化修复（CollapsePDFSpacing）。
const TokenizerVersion = "gse-zh-v2"

// customWords 自定义技术词典：GSE 内置词典的观测切分缺口 + 高频领域词。
// 只收「默认切分会切碎」的词，不追求覆盖面——词典膨胀会拖慢加载与误召回。
var customWords = []string{
	// 观测缺口（GSE 默认词典切错的实锤样本）
	"泛型", "全文检索", "多模态", "大模型", "智能体", "提示词", "工作流",
	"向量检索", "上下文", "端到端", "服务端", "客户端", "桌面端", "移动端",
}

// TextAnalyzer 文本分析器。seg 为 nil 时降级为原文直通（unicode61 按字切分，
// 检索质量下降但功能不炸）——GSE 词典加载失败不应导致文档链路不可用。
type TextAnalyzer struct {
	seg *gse.Segmenter
}

var (
	analyzerOnce   sync.Once
	sharedAnalyzer *TextAnalyzer
)

// SharedAnalyzer 进程级共享实例：gse 词典加载数百毫秒，不可按请求重建。
func SharedAnalyzer() *TextAnalyzer {
	analyzerOnce.Do(func() {
		a, err := NewTextAnalyzer()
		if err != nil {
			log.Warnf("[Absorb] text analyzer init failed, fallback to raw text: %v", err)
			return
		}
		sharedAnalyzer = a
	})
	return sharedAnalyzer
}

// NewTextAnalyzer 构建分析器：加载 GSE 内置中文词典并补自定义词。
func NewTextAnalyzer() (*TextAnalyzer, error) {
	s, err := gse.New("zh")
	if err != nil {
		return nil, err
	}
	seg := &s
	for _, w := range customWords {
		_ = seg.AddToken(w, 100.0) // 频次给高值：确保整词优先于子词切分
	}
	return &TextAnalyzer{seg: seg}, nil
}

// SearchText 索引侧：正文 → 分词文本（空格分隔，交 FTS5 unicode61 索引）。
func (a *TextAnalyzer) SearchText(content string) string {
	tokens := a.tokenize(content)
	return strings.Join(tokens, " ")
}

// QueryExpression 查询侧：用户问题 → FTS5 MATCH 表达式（OR 连接，扩大召回，
// 相关性交给 bm25 排序）。
func (a *TextAnalyzer) QueryExpression(query string) string {
	tokens := a.tokenize(query)
	if len(tokens) == 0 {
		return ""
	}
	quoted := make([]string, 0, len(tokens))
	seen := make(map[string]struct{}, len(tokens))
	for _, t := range tokens {
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		// 双引号短语转义：规范化后 token 理论上不含引号，防御性剥离
		quoted = append(quoted, `"`+strings.ReplaceAll(t, `"`, ``)+`"`)
	}
	return strings.Join(quoted, " OR ")
}

// queryStopwords 查询侧停用词：无区分度的功能词/疑问词，进 OR 只添噪声。
var queryStopwords = map[string]struct{}{
	"的": {}, "了": {}, "是": {}, "在": {}, "和": {}, "与": {}, "或": {}, "对": {},
	"把": {}, "被": {}, "从": {}, "向": {}, "什么": {}, "怎么": {}, "如何": {},
	"这个": {}, "那个": {}, "哪些": {}, "哪些资料": {}, "以及": {}, "对于": {},
}

// latinRuns 拉丁/数字 run 提取：GSE 对未登录的独立英文 token 会整体丢弃
// （实测 CutSearch("GraphRAG") → []，但句中能切出），检索侧不可依赖它保证
// 英文术语可达——正则提取与 GSE 输出取并集。
var latinRuns = regexp.MustCompile(`[a-z0-9][a-z0-9+#./-]*`)

// tokenize 共用管线：规范化 → PDF 空格化修复 → GSE 搜索模式切分 ∪ 拉丁 run 提取 → 过滤。
// （库内旧索引/直读 HTML 的正文无空格化问题，CollapsePDFSpacing 对正常文本幂等，
// 统一过一遍无副作用；查询侧同样过——用户复制粘贴 PDF 文本来问时同款修复生效。）
func (a *TextAnalyzer) tokenize(s string) []string {
	norm := normalizeText(s)
	norm = extract.CollapsePDFSpacing(norm)
	var raw []string
	if a == nil || a.seg == nil {
		// 降级：原文直通（unicode61 会把中文切成单字，检索质量降但可用）
		raw = strings.Fields(norm)
	} else {
		raw = a.seg.CutSearch(norm)
	}
	seen := make(map[string]struct{}, len(raw)+8)
	out := make([]string, 0, len(raw)+8)
	add := func(t string) {
		t = strings.TrimSpace(t)
		if t == "" || isPunctOnly(t) {
			return
		}
		if _, stop := queryStopwords[t]; stop {
			return
		}
		if _, dup := seen[t]; dup {
			return
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	for _, t := range raw {
		add(t)
	}
	for _, t := range latinRuns.FindAllString(norm, -1) {
		add(t)
	}
	return out
}

// normalizeText 规范化：小写（英文统一）→ 全角转半角（含全角空格）→ 空白折叠。
// 不删标点：C++/HTTP/2 等技术标识符依赖标点，删了会破坏词形。
func normalizeText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == 0x3000: // 全角空格
			r = ' '
		case r >= 0xFF01 && r <= 0xFF5E: // 全角 ASCII 区
			r -= 0xFEE0
		}
		b.WriteRune(r)
	}
	return strings.Join(strings.Fields(strings.ToLower(b.String())), " ")
}

// isPunctOnly token 是否只含标点/符号（FTS 无价值）。
func isPunctOnly(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z',
			r >= 0x4E00 && r <= 0x9FFF, // CJK 统一表意文字
			r >= 0x3400 && r <= 0x4DBF: // CJK 扩展 A
			return false
		}
	}
	return true
}
