package extract

// CollapsePDFSpacing 测试：真实语料暴露的 PDF 逐字符空格化（《Go 程序设计
// 语言》中文版整本 "最 早 参 与" / "g o r o u t i n e"）。

import "testing"

func TestCollapsePDFSpacing(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"中文逐字空格化", "最 早 参 与 翻 译 和 审 校", "最早参与翻译和审校"},
		{"英文术语逐字母", "g o r o u t i n e 与 c h a n n e l", "goroutine 与 channel"},
		{"英文术语末字母独立", "同一个 goroutin e 中的队列", "同一个 goroutine 中的队列"},
		{"混合", "使用 g o r o u t i n e 实现 并 发", "使用 goroutine 实现并发"},
		{"正常中文不受影响", "这是正常的中文句子，没有空格问题。", "这是正常的中文句子，没有空格问题。"},
		{"正常英文单词保留空格", "hello world and go routine map", "hello world and go routine map"},
		{"中英边界空格保留", "在 Go 语言中 使 用 c h a n", "在 Go 语言中使用 chan"},
		{"拉丁短序列不压缩", "A B 测试方案", "A B 测试方案"},
		{"换行场景", "最 早 参 与\n翻 译", "最早参与\n翻译"},
		{"空串", "", ""},
	}
	for _, tc := range cases {
		if got := CollapsePDFSpacing(tc.in); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestCollapsePDFSpacingIdempotent 幂等：重复处理不再变化。
func TestCollapsePDFSpacingIdempotent(t *testing.T) {
	in := "g o r o u t i n e 最 早 参 与"
	once := CollapsePDFSpacing(in)
	if twice := CollapsePDFSpacing(once); twice != once {
		t.Fatalf("not idempotent: %q -> %q", once, twice)
	}
}
