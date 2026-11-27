package document

// suggestions_test.go —— 动态建议提问双通道测试（design/doc-suggestions-design.md）：
// 通道 A（marks 层级抽样：位置分布/层级过滤/去重/条数门槛）与通道 B
// （section_path 末级标题 + 首现顺序截取），以及降级链与加载失败静默。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"absorb-agent/internal/document/extract"
	"absorb-agent/internal/store/storage"
)

// mkMarks 构造等距一级章：n 章，均分 total 长度。
func mkMarks(n, total int) []extract.HeadingMark {
	marks := make([]extract.HeadingMark, 0, n)
	for i := 0; i < n; i++ {
		marks = append(marks, extract.HeadingMark{
			Offset: total * i / n,
			Level:  1,
			Title:  "第一章标题" + string(rune('A'+i%26)),
			Path:   []string{"全书"},
		})
	}
	return marks
}

func TestSuggestFromMarksSampling(t *testing.T) {
	// 10 章 × 20000 rune：25%/55%/85% 应命中 3 个不同章（跨书首/中/尾）
	marks := mkMarks(10, 20000)
	qs := suggestFromMarks(marks, 20000)
	if len(qs) != 3 {
		t.Fatalf("期望 3 条建议，得 %d：%v", len(qs), qs)
	}
	seen := map[string]bool{}
	for _, q := range qs {
		if !strings.HasPrefix(q, "书中〈") || !strings.HasSuffix(q, "？") {
			t.Fatalf("组句模板不符：%q", q)
		}
		seen[q] = true
	}
	if len(seen) != 3 {
		t.Fatalf("抽样去重后应 3 条互异：%v", qs)
	}
}

func TestSuggestFromMarksLevelFilter(t *testing.T) {
	// Level 3 小节应被过滤：仅 1 个一级章 → 产出不足走通道 B（返回 nil）
	marks := []extract.HeadingMark{
		{Offset: 0, Level: 1, Title: "唯一的章"},
		{Offset: 10, Level: 3, Title: "小节"},
		{Offset: 20, Level: 3, Title: "小节"},
	}
	if qs := suggestFromMarks(marks, 1000); qs != nil {
		t.Fatalf("一级章不足 2，应返回 nil：%v", qs)
	}
}

func TestSuggestFromMarksShortBookDedup(t *testing.T) {
	// 2 章短书：三个抽样位置可能命中同章，去重后 ≥2 才产出
	marks := mkMarks(2, 1000)
	qs := suggestFromMarks(marks, 1000)
	if len(qs) < 2 {
		t.Fatalf("2 章书应至少产出 2 条：%v", qs)
	}
}

func TestBuildSuggestionsChannelATakesPriority(t *testing.T) {
	dc := &storage.DocumentContent{
		CharCount:    20000,
		HeadingMarks: `[{"offset":0,"level":1,"title":"第一章"},{"offset":9000,"level":1,"title":"第二章"},{"offset":17000,"level":1,"title":"第三章"}]`,
	}
	loaderErr := errors.New("通道 A 命中时不应调用 loader")
	qs := buildSuggestions(context.Background(), "app", "user", "doc1", dc,
		func(context.Context, string, string, string) ([]string, error) { return nil, loaderErr })
	if len(qs) != 3 {
		t.Fatalf("通道 A 应产出 3 条：%v", qs)
	}
	for _, q := range qs {
		if !strings.HasPrefix(q, "书中〈") {
			t.Fatalf("应使用书中模板：%q", q)
		}
	}
}

func TestBuildSuggestionsChannelBFallback(t *testing.T) {
	// marks 不足（仅 1 个一级章）→ 降级通道 B：section_path 末级标题、首现顺序截取
	dc := &storage.DocumentContent{
		CharCount:    1000,
		HeadingMarks: `[{"offset":0,"level":1,"title":"唯一的章"}]`,
	}
	sections := []string{
		"前言 > 安装 > 配置", // 末级「配置」
		"快速开始",           // 无分隔符，整串即末级
		"",
		"深入 > 并发模型",
		"深入 > 测试",
	}
	qs := buildSuggestions(context.Background(), "app", "user", "doc1", dc,
		func(context.Context, string, string, string) ([]string, error) { return sections, nil })
	if len(qs) != 3 {
		t.Fatalf("通道 B 应产出 3 条（空 path 过滤后取前 3）：%v", qs)
	}
	for i, want := range []string{"配置", "快速开始", "并发模型"} {
		if !strings.Contains(qs[i], "〈"+want+"〉") || !strings.HasPrefix(qs[i], "文中〈") {
			t.Fatalf("第 %d 条应为文中模板且含末级标题 %q：%q", i, want, qs[i])
		}
	}
}

func TestBuildSuggestionsBothEmpty(t *testing.T) {
	dc := &storage.DocumentContent{CharCount: 100}
	qs := buildSuggestions(context.Background(), "app", "user", "doc1", dc,
		func(context.Context, string, string, string) ([]string, error) { return []string{"仅一条"}, nil })
	if qs != nil {
		t.Fatalf("双通道皆不足应返回 nil：%v", qs)
	}
}

func TestBuildSuggestionsLoaderErrorSilent(t *testing.T) {
	dc := &storage.DocumentContent{CharCount: 100}
	qs := buildSuggestions(context.Background(), "app", "user", "doc1", dc,
		func(context.Context, string, string, string) ([]string, error) { return nil, errors.New("boom") })
	if qs != nil {
		t.Fatalf("加载失败应静默返回 nil：%v", qs)
	}
}
