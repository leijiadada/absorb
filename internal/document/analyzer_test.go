package document

// TextAnalyzer 测试：规范化稳定性、GSE 切分缺口回归（英文术语独立成查）、查询表达式合法性。

import (
	"strings"
	"testing"
)

func newAnalyzer(t *testing.T) *TextAnalyzer {
	t.Helper()
	a, err := NewTextAnalyzer()
	if err != nil {
		t.Fatalf("analyzer: %v", err)
	}
	return a
}

// TestAnalyzer_LatinRunFallback GSE 对未登录独立英文 token 整体丢弃的回归守卫：
// 查询 "GraphRAG" 必须产出 token，否则该术语检索静默零命中（实测缺陷）。
func TestAnalyzer_LatinRunFallback(t *testing.T) {
	a := newAnalyzer(t)
	expr := a.QueryExpression("GraphRAG")
	if expr == "" || !strings.Contains(expr, "graphrag") {
		t.Fatalf("latin fallback missing: %q", expr)
	}
	// 索引侧同样需要：独立成段的英文标题不应被丢弃
	if !strings.Contains(a.SearchText("GraphRAG"), "graphrag") {
		t.Fatalf("index side latin fallback missing")
	}
}

// TestAnalyzer_QueryExpression 双词 OR 表达式 + 停用词过滤。
func TestAnalyzer_QueryExpression(t *testing.T) {
	a := newAnalyzer(t)
	expr := a.QueryExpression("软件工程的核心原则是什么")
	for _, want := range []string{`"软件"`, `"工程"`, `"核心"`, `"原则"`} {
		if !strings.Contains(expr, want) {
			t.Fatalf("expr missing %q: %s", want, expr)
		}
	}
	if strings.Contains(expr, `"的"`) || strings.Contains(expr, `"什么"`) {
		t.Fatalf("stopwords must be filtered: %s", expr)
	}
	if !strings.Contains(expr, " OR ") {
		t.Fatalf("terms must be OR-joined: %s", expr)
	}
}

// TestAnalyzer_Normalize 全角→半角、小写、空白折叠；标点保留（技术标识符）。
func TestAnalyzer_Normalize(t *testing.T) {
	a := newAnalyzer(t)
	got := a.SearchText("Ｇｏｏｇｌｅ　Ｆｕｃｈｓｉａ")
	if !strings.Contains(got, "google") || !strings.Contains(got, "fuchsia") {
		t.Fatalf("fullwidth not normalized: %q", got)
	}
	// C++ 的词形依赖标点：tokenize 不应产出空（标识符保护的基础）
	if a.QueryExpression("C++") == "" {
		t.Fatal("identifier-only query must stay searchable")
	}
}

// TestAnalyzer_Empty 空输入产出空表达式（FTS MATCH '' 会报错，必须前置拦截）。
func TestAnalyzer_Empty(t *testing.T) {
	a := newAnalyzer(t)
	if expr := a.QueryExpression("   "); expr != "" {
		t.Fatalf("empty query expr = %q", expr)
	}
	if expr := a.QueryExpression("的了是在和"); expr != "" {
		t.Fatalf("stopword-only expr = %q", expr)
	}
}
