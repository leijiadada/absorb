package chat

import (
	"strings"
	"testing"

	"absorb-agent/internal/knowledge"
)

// parseDraftOutput 纯函数测试：模型输出形态各异（围栏/前后缀/纯 JSON），
// 解析必须稳健；垃圾输出报错交上层降级。
func TestParseDraftOutput(t *testing.T) {
	cases := []struct {
		name, in, title, summary string
		wantErr                  bool
	}{
		{name: "clean json", in: `{"title":"标题","summary":"提炼"}`, title: "标题", summary: "提炼"},
		{name: "fenced", in: "```json\n{\"title\":\"标题\",\"summary\":\"提炼\"}\n```", title: "标题", summary: "提炼"},
		{name: "prefixed prose", in: "好的，结果如下：\n{\"title\":\"标题\",\"summary\":\"提炼\"}", title: "标题", summary: "提炼"},
		{name: "empty fields", in: `{"title":"","summary":""}`, title: "", summary: ""}, // 空标题=降级信号，非解析错误
		{name: "garbage", in: "模型不想输出 JSON", wantErr: true},
		{name: "empty output", in: "", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := parseDraftOutput(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want error, got %+v", res)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Title != c.title || res.Summary != c.summary {
				t.Fatalf("got %+v, want title=%q summary=%q", res, c.title, c.summary)
			}
		})
	}
}

// draftUserPrompt 材料组装：摘录可缺省。
func TestDraftUserPrompt(t *testing.T) {
	full := draftUserPrompt(knowledge.DraftPrompt{Question: "Q", Answer: "A", Quote: "Z"})
	for _, seg := range []string{"【问题】", "Q", "【AI 回答】", "A", "【原文摘录】", "Z"} {
		if !strings.Contains(full, seg) {
			t.Fatalf("prompt missing %q:\n%s", seg, full)
		}
	}
	noQuote := draftUserPrompt(knowledge.DraftPrompt{Question: "Q", Answer: "A"})
	if strings.Contains(noQuote, "【原文摘录】") {
		t.Fatalf("empty quote should omit section:\n%s", noQuote)
	}
}

// 系统提示词必须包含防幻觉与输出格式约束（改提示词时两处一致是产品约定）。
func TestDraftSystemPromptContract(t *testing.T) {
	for _, kw := range []string{"JSON", "20 字", "120 字", "置空"} {
		if !strings.Contains(knowledge.DraftSystemPrompt, kw) {
			t.Fatalf("system prompt missing %q", kw)
		}
	}
}
