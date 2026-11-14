//go:build !integration

package main

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain 检测 goroutine 泄漏：本包会在测试中装配 runner/会话服务，
// 关闭路径漏掉后台协程时在这里暴露，而不是留给下一次测试互相污染。
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m, goleak.IgnoreCurrent())
}

// TestVisualCase_evaluate 断言逻辑自身的单测：不需要真实模型即可覆盖
// 容器判定、选型断言、多写法命中与表格列数上限，避免回归测试被错误的断言误导。
func TestVisualCase_evaluate(t *testing.T) {
	const container = ":::absorb-visual\n---\ntemplate: sheet\n---\n## 面板\n"

	tests := []struct {
		name   string
		c      visualCase
		answer string
		wantOK bool
	}{
		{
			name:   "容器与选型断言齐全",
			c:      visualCase{wantVisual: true, mustContain: []string{"sequenceDiagram"}},
			answer: container + "sequenceDiagram\n",
			wantOK: true,
		},
		{
			name:   "要求可视化但缺容器",
			c:      visualCase{wantVisual: true},
			answer: "纯文本回答",
			wantOK: false,
		},
		{
			name:   "简单题不应出现容器",
			c:      visualCase{wantVisual: false},
			answer: container,
			wantOK: false,
		},
		{
			name:   "简单题无容器",
			c:      visualCase{wantVisual: false},
			answer: "纯文本回答",
			wantOK: true,
		},
		{
			name:   "简单题答非所问",
			c:      visualCase{wantVisual: false, mustContain: []string{"Redis"}},
			answer: "我不知道",
			wantOK: false,
		},
		{
			name:   "简单题命中关键词",
			c:      visualCase{wantVisual: false, mustContain: []string{"Redis"}},
			answer: "Redis 是内存数据库",
			wantOK: true,
		},
		{
			name:   "选型要素缺失",
			c:      visualCase{wantVisual: true, mustContain: []string{"sequenceDiagram"}},
			answer: container + "flowchart\n",
			wantOK: false,
		},
		{
			name:   "anyOf 命中第二种写法",
			c:      visualCase{wantVisual: true, anyOf: []string{"```chart", ":::chart"}},
			answer: container + ":::chart bar\n",
			wantOK: true,
		},
		{
			name:   "anyOf 全未命中",
			c:      visualCase{wantVisual: true, anyOf: []string{"```chart", ":::chart"}},
			answer: container + "```mermaid\n",
			wantOK: false,
		},
		{
			name:   "表格列数超限",
			c:      visualCase{wantVisual: true, maxTableDataCols: 4},
			answer: container + "| a | b | c | d | e |\n",
			wantOK: false,
		},
		{
			name:   "表格列数合规",
			c:      visualCase{wantVisual: true, maxTableDataCols: 4},
			answer: container + "| a | b | c | d |\n",
			wantOK: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, asserts := tt.c.evaluate(tt.answer)
			if ok != tt.wantOK {
				t.Errorf("evaluate() = %v, want %v (asserts=%v)", ok, tt.wantOK, asserts)
			}
		})
	}
}

// TestRegressionCases_Shape 用例清单的结构约束：id 唯一、问题非空，
// 且同时覆盖"该可视化"与"不该可视化"两类，防止清单被改坏后回归失去意义。
func TestRegressionCases_Shape(t *testing.T) {
	cases := regressionCases()
	if len(cases) == 0 {
		t.Fatal("回归用例清单为空")
	}
	ids := make(map[string]bool, len(cases))
	visual, plain := 0, 0
	for _, c := range cases {
		if c.id == "" {
			t.Errorf("存在空 id 的用例: %+v", c)
		}
		if ids[c.id] {
			t.Errorf("用例 id 重复: %q", c.id)
		}
		ids[c.id] = true
		if c.question == "" {
			t.Errorf("用例 %q 缺少问题", c.id)
		}
		if c.wantVisual {
			visual++
		} else {
			plain++
		}
	}
	if visual == 0 || plain == 0 {
		t.Errorf("用例应同时覆盖正反两类，got visual=%d plain=%d", visual, plain)
	}
}
