package extract

// poppler 测试：择优打分器（含 Phase 0 实测校准数据回归）+ pdftotext 真实
// 提取冒烟（bin 缺失自动 Skip，不进常规 CI 硬门槛）。

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestPickBetterText(t *testing.T) {
	cases := []struct {
		name       string
		current    string
		currentBad bool
		alt        string
		want       string
		wantSwitch bool
	}{
		{"主路乱码必选第二引擎", strings.Repeat("\uFFFD", 600), true, strings.Repeat("正文内容。", 100), strings.Repeat("正文内容。", 100), true},
		{"第二引擎为空保持主路", strings.Repeat("正文内容。", 100), false, "", strings.Repeat("正文内容。", 100), false},
		{"主路为空必选第二引擎", "", false, strings.Repeat("正文内容。", 100), strings.Repeat("正文内容。", 100), true},
		{"行内乱序显著更优才切换", interleaveSample(), false, cleanSample(), cleanSample(), true},
		{"打平保持主路", strings.Repeat("正文内容。", 100), false, strings.Repeat("正文内容。", 100), strings.Repeat("正文内容。", 100), false},
	}
	for _, c := range cases {
		got, switched := pickBetterText(c.current, c.currentBad, c.alt)
		if got != c.want || switched != c.wantSwitch {
			t.Fatalf("%s: got(switch=%v) want(switch=%v)", c.name, switched, c.wantSwitch)
		}
	}
}

// cleanSample 干净真实感语料（≥500 汉字，触发乱序项）。
func cleanSample() string {
	return strings.Repeat("软件工程的目标是在规定的时间和预算内交付满足用户需求的可靠软件，团队协作与代码评审是质量的基石。", 12)
}

// interleaveSample dslipak 行内乱序语料：相邻两句逐字符交错组装（P1 实验
// 观察到的真实形态，如"中克。数知先集未力量攻"）。
func interleaveSample() string {
	const a = "软件工程的目标是在规定的时间和预算内交付满足用户需求的可靠软件。"
	const b = "团队协作与代码评审是质量的基石，工具链无法替代人对设计的判断。"
	var bld strings.Builder
	for i := 0; i < 12; i++ {
		ra, rb := []rune(a), []rune(b)
		for j := 0; j < len(ra) || j < len(rb); j++ {
			if j < len(ra) {
				bld.WriteRune(ra[j])
			}
			if j < len(rb) {
				bld.WriteRune(rb[j])
			}
		}
		bld.WriteString("\n")
	}
	return bld.String()
}

// TestRareBigramRate 乱序率直测：干净语料低、交错语料显著更高（绝对差
// >0.1）、短文本返回 0（噪声门）。
func TestRareBigramRate(t *testing.T) {
	clean := cleanSample()
	inter := interleaveSample()
	rc, ri := rareBigramRate(clean), rareBigramRate(inter)
	if len([]rune(strings.ReplaceAll(clean, "\n", ""))) < 500 {
		t.Fatal("cleanSample 汉字量不足，乱序项未生效")
	}
	if ri-rc < 0.1 {
		t.Fatalf("乱序区分度不足：clean=%.3f interleave=%.3f", rc, ri)
	}
	if short := rareBigramRate("短文本样本不足以支撑二元组统计。"); short != 0 {
		t.Fatalf("短文本应返回 0：%.3f", short)
	}
}

// TestTextQualityScoreOrderingCalibration P1 实验回归：行内乱序（dslipak
// 组装缺陷，AgenticSE 实测罕见 bigram +9.6pp）必须让择优器切到干净文本——
// 乱码率与逐字空格率两项对此失明（Collapse 后均≈0），乱序项是唯一信号。
func TestTextQualityScoreOrderingCalibration(t *testing.T) {
	current, alt := interleaveSample(), cleanSample()
	sCur, sAlt := textQualityScore(current), textQualityScore(alt)
	if sAlt*1.1 >= sCur {
		t.Fatalf("切换判据不满足：current=%.4f alt=%.4f（相对优势不足 10%%）", sCur, sAlt)
	}
	if _, switched := pickBetterText(current, false, alt); !switched {
		t.Fatal("行内乱序场景应切换到第二引擎")
	}
}

// findRepoPath 从当前目录向上逐级查找仓库内相对路径（go test 的 cwd 因
// 运行方式而异，绝对化仓库根最稳妥）。
func findRepoPath(rel string) string {
	dir, _ := os.Getwd()
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(dir + "/" + rel); err == nil {
			return dir + "/" + rel
		}
		dir += "/.."
	}
	return ""
}

// TestPopplerTextReal 真书冒烟：仓库 docs/pdf 下的《人月神话》文字版是主路
// 乱码拒绝的代表样本，pdftotext 应完整救回（FFFD 率近零、文本量充分）。
// pdftotext 缺失或样书不存在时 Skip。
func TestPopplerTextReal(t *testing.T) {
	bin := pdftotextBin()
	if bin == "" {
		t.Skip("pdftotext 不可用（未装 Poppler 且未设 ABSORB_PDFTOTEXT），跳过")
	}
	book := findRepoPath("docs/pdf/人月神话-中文-目录-文字版.pdf")
	if book == "" {
		t.Skip("样书不存在（docs/pdf/人月神话-中文-目录-文字版.pdf），跳过")
	}
	data, err := os.ReadFile(book)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text, err := PopplerText(context.Background(), bin, data)
	if err != nil {
		t.Fatalf("PopplerText: %v", err)
	}
	rs := []rune(text)
	if len(rs) < 100000 {
		t.Fatalf("文本量不足：%d rune", len(rs))
	}
	if r := garbledRatio(text); r > 0.005 {
		t.Fatalf("乱码率超标：%.4f", r)
	}
	if !strings.Contains(text, "焦油坑") || !strings.Contains(text, "人月神话") {
		t.Fatal("正文可读性抽检失败")
	}
}
