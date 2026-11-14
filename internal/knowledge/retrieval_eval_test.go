package knowledge

// M3.1 检索评测 harness（评测先行，见 design/m3.1-retrieval-service-design.md §8）。
//
// 数据：design/eval/knowledge-recall/{corpus,cases}.yaml（评测集是规格，实现向它收敛）。
// 本文件职责：
//  1. TestRetrievalEvalContract —— 契约测试：评测集自洽性校验 + harness 在 fake
//     searcher 上跑通并产出报告（results/report.json）。质量门槛（HitRate@3 /
//     负例 FP）不在本测试断言——fake 恒空，门槛断言在实现步的
//     TestRetrievalEvalQuality（以真实 Service + FixedKeywordSource 运行
//     runKnowledgeRecallEval 即可接入，无需改动 harness）。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"absorb-agent/internal/store/storage"
)

// ---- 评测数据常量 ----

const (
	evalDir     = "../../design/eval/knowledge-recall" // 测试 cwd = 包目录（internal/knowledge）
	evalAppName = "absorb-reader"                      // 与生产 AppName / 既有 evalset 一致
	evalUserID  = "eval-user"

	evalCorpusSize = 30 // corpus.yaml 条目数（数据集演进时同步修改）
	evalCaseCount  = 19 // cases.yaml 用例数（10 正例 + 7 严格负例 + 2 缺口观测）
)

// ---- corpus 加载与转换为 storage 对象 ----

type corpusFile struct {
	Meta struct {
		Description string `yaml:"description"`
	} `yaml:"meta"`
	Items []struct {
		ID                string `yaml:"id"`
		Title             string `yaml:"title"`
		AISummary         string `yaml:"ai_summary"`
		UserUnderstanding string `yaml:"user_understanding"`
		Status            string `yaml:"status"`
		Sources           []struct {
			SourceType  string `yaml:"source_type"`
			SourceTitle string `yaml:"source_title"`
			SourceURL   string `yaml:"source_url"`
			QuoteText   string `yaml:"quote_text"`
		} `yaml:"sources"`
	} `yaml:"items"`
}

type evalCorpus struct {
	items    []*storage.KnowledgeItem
	sources  map[string][]*storage.KnowledgeSource // item_id -> sources
	statusOf map[string]string
}

// loadCorpus 读取 corpus.yaml 并转换为 storage 对象；运行时字段
// （app/user/origin/时间戳）以确定性常量补齐，保证评测可复现。
func loadCorpus(t *testing.T) *evalCorpus {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(evalDir, "corpus.yaml"))
	if err != nil {
		t.Fatalf("读取 corpus.yaml 失败: %v", err)
	}
	var cf corpusFile
	if err := yaml.Unmarshal(raw, &cf); err != nil {
		t.Fatalf("解析 corpus.yaml 失败: %v", err)
	}
	if len(cf.Items) != evalCorpusSize {
		t.Fatalf("corpus 条目数 = %d, 期望 %d（数据集演进时同步更新 evalCorpusSize）", len(cf.Items), evalCorpusSize)
	}
	c := &evalCorpus{
		sources:  map[string][]*storage.KnowledgeSource{},
		statusOf: map[string]string{},
	}
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	seen := map[string]bool{}
	for i, it := range cf.Items {
		if it.ID == "" || seen[it.ID] {
			t.Fatalf("corpus 条目 id 非法或重复: %q", it.ID)
		}
		seen[it.ID] = true
		if it.Title == "" || it.AISummary == "" {
			t.Fatalf("corpus 条目 %s 缺 title/ai_summary", it.ID)
		}
		switch it.Status {
		case storage.KnowledgeStatusActive, storage.KnowledgeStatusArchived:
		default:
			t.Fatalf("corpus 条目 %s status 非法: %q", it.ID, it.Status)
		}
		if len(it.Sources) == 0 {
			t.Fatalf("corpus 条目 %s 至少需要 1 条来源", it.ID)
		}
		ts := base.Add(time.Duration(i) * time.Hour)
		c.items = append(c.items, &storage.KnowledgeItem{
			ID:                it.ID,
			AppName:           evalAppName,
			UserID:            evalUserID,
			Title:             it.Title,
			AISummary:         it.AISummary,
			UserUnderstanding: it.UserUnderstanding,
			Status:            it.Status,
			OriginSessionID:   "eval-origin-session",
			OriginEventID:     "eval-origin-" + it.ID,
			CreatedAt:         ts,
			UpdatedAt:         ts,
		})
		c.statusOf[it.ID] = it.Status
		for j, src := range it.Sources {
			switch src.SourceType {
			case storage.KnowledgeSourceQuote:
				if src.QuoteText == "" {
					t.Fatalf("corpus 条目 %s 来源 %d 为 quote 类型，quote_text 必填", it.ID, j)
				}
			case storage.KnowledgeSourceConversation:
				if src.QuoteText != "" {
					t.Fatalf("corpus 条目 %s 来源 %d 为 conversation 类型，quote_text 必须为空", it.ID, j)
				}
			default:
				t.Fatalf("corpus 条目 %s 来源 %d source_type 非法: %q", it.ID, j, src.SourceType)
			}
			c.sources[it.ID] = append(c.sources[it.ID], &storage.KnowledgeSource{
				ID:              fmt.Sprintf("%s-src%d", it.ID, j),
				ItemID:          it.ID,
				AppName:         evalAppName,
				UserID:          evalUserID,
				SourceType:      src.SourceType,
				SourceTitle:     src.SourceTitle,
				SourceURL:       src.SourceURL,
				QuoteText:       src.QuoteText,
				SourceSessionID: "eval-src-session",
				SourceEventID:   "eval-src-" + it.ID,
				CreatedAt:       ts,
			})
		}
	}
	// 数据集形态断言：归档与 quote-only（无 user_understanding）场景必须存在
	hasArchived, hasQuoteOnly := false, false
	for _, it := range c.items {
		if it.Status == storage.KnowledgeStatusArchived {
			hasArchived = true
		}
		if it.UserUnderstanding == "" {
			hasQuoteOnly = true
		}
	}
	if !hasArchived || !hasQuoteOnly {
		t.Fatalf("corpus 必须同时包含 archived 与 quote-only（无 user_understanding）条目")
	}
	return c
}

// ---- cases 加载 ----

type casesFile struct {
	Meta struct {
		Description string `yaml:"description"`
		Thresholds  struct {
			HitAt3     float64 `yaml:"hit_at_3"`
			NegativeFP float64 `yaml:"negative_fp"`
		} `yaml:"thresholds"`
	} `yaml:"meta"`
	Cases []evalCase `yaml:"cases"`
}

type evalCase struct {
	ID           string   `yaml:"id"`
	Category     string   `yaml:"category"`
	Kind         string   `yaml:"kind"`
	PageTitle    string   `yaml:"page_title"`
	Query        string   `yaml:"query"`
	SelectedText string   `yaml:"selected_text"`
	UserQuestion string   `yaml:"user_question"`
	Keywords     []string `yaml:"keywords"`
	ExpectedIDs  []string `yaml:"expected_item_ids"`
	ExpectEmpty  bool     `yaml:"expect_empty"`
	Gap          bool     `yaml:"gap"`
	Note         string   `yaml:"note"`
}

func (c *evalCase) isPositive() bool { return !c.ExpectEmpty && !c.Gap }

func loadCases(t *testing.T) *casesFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(evalDir, "cases.yaml"))
	if err != nil {
		t.Fatalf("读取 cases.yaml 失败: %v", err)
	}
	var cf casesFile
	if err := yaml.Unmarshal(raw, &cf); err != nil {
		t.Fatalf("解析 cases.yaml 失败: %v", err)
	}
	if len(cf.Cases) != evalCaseCount {
		t.Fatalf("用例数 = %d, 期望 %d（数据集演进时同步更新 evalCaseCount）", len(cf.Cases), evalCaseCount)
	}
	if cf.Meta.Thresholds.HitAt3 <= 0 || cf.Meta.Thresholds.HitAt3 > 1 ||
		cf.Meta.Thresholds.NegativeFP < 0 || cf.Meta.Thresholds.NegativeFP >= 1 {
		t.Fatalf("thresholds 非法: %+v", cf.Meta.Thresholds)
	}
	return &cf
}

// validateCases 用例自洽性校验：kind/关键词/期望与 corpus 的交叉引用。
func validateCases(t *testing.T, cf *casesFile, corpus *evalCorpus) {
	t.Helper()
	seen := map[string]bool{}
	for _, c := range cf.Cases {
		if c.ID == "" || seen[c.ID] {
			t.Fatalf("用例 id 非法或重复: %q", c.ID)
		}
		seen[c.ID] = true
		if !RetrievalKind(c.Kind).Valid() {
			t.Fatalf("用例 %s kind 非法: %q", c.ID, c.Kind)
		}
		if len(c.Keywords) == 0 {
			t.Fatalf("用例 %s 缺 keywords（评测固定关键词，绕过规则抽取）", c.ID)
		}
		switch {
		case c.Gap:
			if c.Category != "synonym-gap" || len(c.ExpectedIDs) == 0 || c.ExpectEmpty {
				t.Fatalf("缺口用例 %s 必须 category=synonym-gap 且有期望条目", c.ID)
			}
		case c.ExpectEmpty:
			switch c.Category {
			case "no-history", "archived-exact", "cross-domain":
			default:
				t.Fatalf("严格负例 %s category 非法: %q", c.ID, c.Category)
			}
			if len(c.ExpectedIDs) != 0 {
				t.Fatalf("严格负例 %s 不得携带 expected_item_ids", c.ID)
			}
		default:
			switch c.Category {
			case "support", "extend", "contradict", "quote-only", "summary-goal", "compare":
			default:
				t.Fatalf("正例 %s category 非法: %q", c.ID, c.Category)
			}
			if len(c.ExpectedIDs) == 0 {
				t.Fatalf("正例 %s 缺 expected_item_ids", c.ID)
			}
		}
		for _, id := range c.ExpectedIDs {
			status, ok := corpus.statusOf[id]
			if !ok {
				t.Fatalf("用例 %s 期望条目 %s 不在 corpus 中", c.ID, id)
			}
			if status != storage.KnowledgeStatusActive {
				t.Fatalf("用例 %s 期望条目 %s 不是 active（归档条目不得作为期望）", c.ID, id)
			}
		}
	}
}

// ---- 评测执行与报告 ----

type caseResult struct {
	ID       string   `json:"id"`
	Category string   `json:"category"`
	Passed   bool     `json:"passed"`
	Top3     []string `json:"top3"`
	Expected []string `json:"expected,omitempty"`
	Matched  []string `json:"matched,omitempty"`
	Called   bool     `json:"called"` // searcher 是否被实际调用（契约模式 fake 也会调用）
}

type evalReport struct {
	Mode        string `json:"mode"` // contract | quality
	Searcher    string `json:"searcher"`
	GeneratedAt string `json:"generated_at"`
	CorpusSize  int    `json:"corpus_size"`
	CaseCount   int    `json:"case_count"`
	Metrics     struct {
		HitAt3     float64 `json:"hit_at_3"`
		NegativeFP float64 `json:"negative_fp_rate"`
		GapFound   float64 `json:"gap_found_rate"`
		Thresholds struct {
			HitAt3     float64 `json:"hit_at_3"`
			NegativeFP float64 `json:"negative_fp"`
		} `json:"thresholds"`
	} `json:"metrics"`
	Cases []caseResult `json:"cases"`
	Note  string       `json:"note"`
}

// runKnowledgeRecallEval 评测主入口：以固定关键词逐例运行 searcher，计算指标并落盘报告。
// 契约测试以 fake 运行（仅校验 harness 与数据）；质量门槛断言在实现步的
// TestRetrievalEvalQuality 中以真实 Service 工厂接入后开启。
func runKnowledgeRecallEval(t *testing.T, mode, searcherName string, factory func(ks KeywordSource) RetrievalSearcher) *evalReport {
	t.Helper()
	corpus := loadCorpus(t)
	cf := loadCases(t)
	validateCases(t, cf, corpus)

	rep := &evalReport{Mode: mode, Searcher: searcherName, GeneratedAt: time.Now().UTC().Format(time.RFC3339), CorpusSize: len(corpus.items), CaseCount: len(cf.Cases)}
	rep.Metrics.Thresholds.HitAt3 = cf.Meta.Thresholds.HitAt3
	rep.Metrics.Thresholds.NegativeFP = cf.Meta.Thresholds.NegativeFP

	var posTotal, posHit, negTotal, negFP, gapTotal, gapFound int
	for _, c := range cf.Cases {
		q := RetrievalQuery{
			Kind:         RetrievalKind(c.Kind),
			Query:        c.Query,
			PageTitle:    c.PageTitle,
			SelectedText: c.SelectedText,
			UserQuestion: c.UserQuestion,
		}
		cands, _, err := factory(NewFixedKeywordSource(c.Keywords)).SearchRelevant(
			context.Background(), evalAppName, evalUserID, q, RetrievalOptions{})
		if err != nil {
			t.Fatalf("用例 %s 检索失败: %v", c.ID, err)
		}
		res := caseResult{ID: c.ID, Category: c.Category, Called: true, Expected: c.ExpectedIDs}
		for i, cand := range cands {
			if i < 3 {
				res.Top3 = append(res.Top3, cand.ItemID)
			}
		}
		switch {
		case c.isPositive():
			posTotal++
			res.Matched = intersect(c.ExpectedIDs, res.Top3)
			res.Passed = len(res.Matched) > 0
			if res.Passed {
				posHit++
			}
		case c.Gap:
			gapTotal++
			res.Matched = intersect(c.ExpectedIDs, res.Top3)
			res.Passed = len(res.Matched) > 0 // 缺口例只报告，不计门槛
			if res.Passed {
				gapFound++
			}
		default: // 严格负例
			negTotal++
			res.Passed = len(cands) == 0
			if !res.Passed {
				negFP++
			}
		}
		rep.Cases = append(rep.Cases, res)
	}
	if posTotal > 0 {
		rep.Metrics.HitAt3 = float64(posHit) / float64(posTotal)
	}
	if negTotal > 0 {
		rep.Metrics.NegativeFP = float64(negFP) / float64(negTotal)
	}
	if gapTotal > 0 {
		rep.Metrics.GapFound = float64(gapFound) / float64(gapTotal)
	}

	outDir := filepath.Join(evalDir, "results")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("创建结果目录失败: %v", err)
	}
	out, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatalf("序列化报告失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "report.json"), out, 0o644); err != nil {
		t.Fatalf("写报告失败: %v", err)
	}
	return rep
}

// intersect 求交集并保持 expected 顺序（matched 归因展示用）。
func intersect(expected, top3 []string) []string {
	inTop := map[string]bool{}
	for _, id := range top3 {
		inTop[id] = true
	}
	var out []string
	for _, id := range expected {
		if inTop[id] {
			out = append(out, id)
		}
	}
	return out
}

// ---- fake searcher 与契约测试 ----

// fakeSearcher 恒返回空结果：模拟"实现未落地"状态（M3.1 第 1 步）。
type fakeSearcher struct{}

func (fakeSearcher) SearchRelevant(context.Context, string, string, RetrievalQuery, RetrievalOptions) ([]*RetrievalCandidate, []string, error) {
	return nil, nil, nil
}

// TestRetrievalEvalContract 契约测试：评测集自洽 + harness 跑通 + 报告落盘。
// 质量门槛在此不断言（fake 恒空）；实现步由 TestRetrievalEvalQuality 接管。
func TestRetrievalEvalContract(t *testing.T) {
	rep := runKnowledgeRecallEval(t, "contract", "fake(empty)", func(KeywordSource) RetrievalSearcher {
		return fakeSearcher{}
	})
	// fake 恒空下的指标形态断言：负例全过（FP=0）、正例全不命中、缺口未发现
	if rep.Metrics.HitAt3 != 0 {
		t.Errorf("fake 下 HitAt3 = %v, 期望 0", rep.Metrics.HitAt3)
	}
	if rep.Metrics.NegativeFP != 0 {
		t.Errorf("fake 下 NegativeFP = %v, 期望 0（空结果满足所有严格负例）", rep.Metrics.NegativeFP)
	}
	if rep.Metrics.GapFound != 0 {
		t.Errorf("fake 下 GapFound = %v, 期望 0", rep.Metrics.GapFound)
	}
	t.Logf("契约评测完成: corpus=%d cases=%d HitAt3=%.2f NegativeFP=%.2f GapFound=%.2f",
		rep.CorpusSize, rep.CaseCount, rep.Metrics.HitAt3, rep.Metrics.NegativeFP, rep.Metrics.GapFound)
}

// TestRetrievalEvalQuality 质量门槛测试（M3.1 第 4 步）：真实实现——sqlite 内存库
// + storage LIKE 预筛 + 字段权重打分 + 固定关键词（评测纪律），断言 cases.yaml
// 声明的门槛：HitRate@3 ≥ hit_at_3，严格负例误报率 ≤ negative_fp。
func TestRetrievalEvalQuality(t *testing.T) {
	corpus := loadCorpus(t)
	st, err := storage.New("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	for _, it := range corpus.items {
		item := *it
		item.DedupKey = "dk-" + it.ID // 幂等锚点在评测中无语义，保证唯一防 INSERT OR IGNORE 跳过
		if _, _, err := st.CreateKnowledge(ctx, it.AppName, it.UserID, &item, corpus.sources[it.ID]); err != nil {
			t.Fatalf("写入语料 %s 失败: %v", it.ID, err)
		}
	}
	rep := runKnowledgeRecallEval(t, "quality", "sqlite+rule-score", func(ks KeywordSource) RetrievalSearcher {
		svc := &Service{st: st}
		svc.SetKeywordSource(ks)
		return svc
	})
	cf := loadCases(t)
	if rep.Metrics.HitAt3 < cf.Meta.Thresholds.HitAt3 {
		t.Errorf("HitAt3 = %.2f，低于门槛 %.2f", rep.Metrics.HitAt3, cf.Meta.Thresholds.HitAt3)
	}
	if rep.Metrics.NegativeFP > cf.Meta.Thresholds.NegativeFP {
		t.Errorf("严格负例误报率 = %.2f，超过门槛 %.2f", rep.Metrics.NegativeFP, cf.Meta.Thresholds.NegativeFP)
	}
	for _, cr := range rep.Cases {
		if !cr.Passed && cr.Category != "synonym-gap" {
			t.Logf("MISS %s (%s) top3=%v expected=%v", cr.ID, cr.Category, cr.Top3, cr.Expected)
		}
	}
	t.Logf("质量评测: HitAt3=%.2f(门槛 %.2f) NegativeFP=%.2f(门槛 %.2f) GapFound=%.2f",
		rep.Metrics.HitAt3, cf.Meta.Thresholds.HitAt3,
		rep.Metrics.NegativeFP, cf.Meta.Thresholds.NegativeFP, rep.Metrics.GapFound)
}
