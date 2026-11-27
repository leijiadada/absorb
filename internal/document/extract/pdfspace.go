// PDF 文本层空格化修复（实测案例：《Go 程序设计语言》中文版提取结果
// "最 早 参 与 翻 译"、"g o r o u t i n e"——CID 字体把每个字符输出为独立
// fragment，httpfetch 的 pdfPageText 按坐标间距插空格，导致中文被逐字切开、
// 英文术语被逐字母切开。分词与原文呈现两侧都不可用，必须在产品层压缩）。
package extract

import (
	"regexp"
	"strings"
	"unicode"
)

var (
	// cjkSpaceGap 相邻 CJK 字符间的空格：中文词内空格无语言学意义，一律删除。
	// 用迭代替换处理重叠对（"你 好 世 界" 单遍正则只能命中不相邻的对）。
	cjkChar = func(r rune) bool {
		return unicode.Is(unicode.Han, r) || r == '々' || r == '〆'
	}
	// letterSpaceRun 连续「小写字母/数字+空格」序列：逐字母切开的英文术语
	// （"g o r o u t i n e" 全切开、或 "goroutin e" 仅末字母独立——实测两者都有）。
	// 只压小写/数字：大写单字母（"A B 测试"、人名缩写）是合法形态不碰；
	// 正常多字母单词（"hello world"）不受影响。
	letterSpaceRun = regexp.MustCompile(`(?:[a-z0-9] ){1,}[a-z0-9]`)
)

// CollapsePDFSpacing 压缩 PDF 文本层的逐字符空格化：
//  1. CJK 字符之间的空格全部删除；
//  2. 连续单字母+空格序列（≥3 字符）内部空格删除。
//
// 幂等：已正常文本（单词间空格、正常中文）不变。
func CollapsePDFSpacing(s string) string {
	if s == "" {
		return s
	}
	if !looksSpaceSplit(s) {
		return s
	}
	s = collapseCJKSpaces(s)
	s = letterSpaceRun.ReplaceAllStringFunc(s, func(run string) string {
		return strings.ReplaceAll(run, " ", "")
	})
	return s
}

// looksSpaceSplit 快速判定是否值得做压缩扫描：存在「CJK 后跟空格」即疑似。
func looksSpaceSplit(s string) bool {
	prevCJK := false
	for _, r := range s {
		if prevCJK && (r == ' ' || r == '\t') {
			return true
		}
		prevCJK = cjkChar(r)
	}
	return false
}

// collapseCJKSpaces 删除 CJK 字符之间的空格（不动 CJK 与拉丁/标点之间的空格）。
func collapseCJKSpaces(s string) string {
	rs := []rune(s)
	out := rs[:0]
	for i, r := range rs {
		if r == ' ' && i > 0 && i < len(rs)-1 && cjkChar(rs[i-1]) && cjkChar(rs[i+1]) {
			continue // CJK-CJK 之间的空格丢弃
		}
		out = append(out, r)
	}
	return string(out)
}

// garbledRatio 乱码占比：U+FFFD 替换符与不可打印控制字符占全部字符的比例。
// 字体缺 ToUnicode cmap 的中文 PDF 提取结果大量落在这个区间（实测接近 100%）。
func garbledRatio(s string) float64 {
	if s == "" {
		return 0
	}
	total, bad := 0, 0
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' {
			continue
		}
		total++
		if r == unicode.ReplacementChar || (r < 0x20 && r != 0) || (r >= 0xE000 && r <= 0xF8FF) {
			bad++
		}
	}
	if total == 0 {
		return 0
	}
	return float64(bad) / float64(total)
}
