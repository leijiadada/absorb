package readpage

// read_page 工具测试：章节切分/目录/按节读取/预算截断/无缓存降级/越界提示。
import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func mustJSON(in readPageInput) []byte {
	raw, err := json.Marshal(in)
	if err != nil {
		panic(err)
	}
	return raw
}

const samplePage = "导语段落，交代全文主题。\n\n## 背景\n背景章节的完整内容。\n\n### 背景-细节\n细节内容。\n\n## 方案设计\n方案章节内容。"

func withPage(text string) context.Context {
	return WithPageSectionState(context.Background(), text)
}

func call(t *testing.T, ctx context.Context, in readPageInput) readPageOutput {
	t.Helper()
	res, err := NewReadPageTool().Call(ctx, mustJSON(in))
	if err != nil {
		t.Fatalf("tool call: %v", err)
	}
	out, ok := res.(readPageOutput)
	if !ok {
		t.Fatalf("result type = %T", res)
	}
	return out
}

// TestReadPageOutline 无参调用返回章节目录（序号 + 层级 + 标题 + 字数）。
func TestReadPageOutline(t *testing.T) {
	out := call(t, withPage(samplePage), readPageInput{})
	if !out.Success {
		t.Fatalf("expect success: %+v", out)
	}
	for _, want := range []string{"章节目录", "1. 导语", "## 背景", "### 背景-细节", "## 方案设计"} {
		if !strings.Contains(out.Content, want) {
			t.Fatalf("outline missing %q:\n%s", want, out.Content)
		}
	}
}

// TestReadPageSection 按序号读取章节全文。
func TestReadPageSection(t *testing.T) {
	out := call(t, withPage(samplePage), readPageInput{Section: 2})
	if !out.Success {
		t.Fatalf("expect success: %+v", out)
	}
	if !strings.Contains(out.Content, "【第 2 节】背景") || !strings.Contains(out.Content, "背景章节的完整内容。") {
		t.Fatalf("section content missing:\n%s", out.Content)
	}
	if strings.Contains(out.Content, "细节内容") {
		t.Fatalf("H3 子节不应混入 H2 节:\n%s", out.Content)
	}
}

// TestReadPagePreamble 首个标题前的非空内容归入"导语"节。
func TestReadPagePreamble(t *testing.T) {
	out := call(t, withPage(samplePage), readPageInput{Section: 1})
	if !out.Success || !strings.Contains(out.Content, "导语段落") {
		t.Fatalf("preamble missing:\n%s", out.Content)
	}
}

// TestReadPageNoCache 无缓存页面 → 明确失败信号。
func TestReadPageNoCache(t *testing.T) {
	out := call(t, withPage(""), readPageInput{})
	if out.Success || !strings.Contains(out.Message, "没有缓存的页面内容") {
		t.Fatalf("no cache mismatch: %+v", out)
	}
}

// TestReadSectionOutOfRange 序号越界 → 提示实际章节数。
func TestReadSectionOutOfRange(t *testing.T) {
	out := call(t, withPage(samplePage), readPageInput{Section: 9})
	if out.Success || !strings.Contains(out.Message, "共 4 个章节") {
		t.Fatalf("out of range mismatch: %+v", out)
	}
}

// TestReadSectionBudget 超预算章节截断并提示。
func TestReadSectionBudget(t *testing.T) {
	long := "## 长章节\n" + strings.Repeat("长", 6500)
	out := call(t, withPage(long), readPageInput{Section: 1})
	if !out.Success || !strings.Contains(out.Content, "已截断") {
		t.Fatalf("budget truncation missing: %+v", out)
	}
}

// TestSplitSectionsHeadingLevel 标题级别识别：# 到 ###### 有效，非标题行返回 0。
func TestSplitSectionsHeadingLevel(t *testing.T) {
	cases := map[string]int{
		"# 一级":       1,
		"###### 六级":  6,
		"####### 七级": 0, // 7 个 # 不是标题
		"#NoSpace":   0, // # 后无空格
		"正文段落":       0,
		"  ## 缩进标题":  0, // 缩进行不作为标题（markdown 规范）
	}
	for line, want := range cases {
		if got := headingLevel(line); got != want {
			t.Fatalf("headingLevel(%q) = %d, want %d", line, got, want)
		}
	}
}
