// poppler.go PDF 文本提取的 poppler(pdftotext) 第二引擎与双跑择优。
//
// Phase 0 实测（6 本中文技术 PDF，docling v1.6.2 / pdftotext 26.09）：
// pdftotext 的文本质量全面优于 dslipak 文本层——两本缺 ToUnicode 的书
// （《人月神话》《程序员的自我修养》）从"拒绝入库"变为完整可读（FFFD=0）、
// 《Go 程序设计语言》的 CID 逐字空格消失、AgenticSE 残留 CJK 空格 1216→72。
// docling 同样内置该后端，但其页级 15% 乱码触发门槛对"局部乱码"语料永不
// 生效，故 Absorb 直接进程调用 pdftotext。
//
// 择优策略（双跑）：主路（httpfetch/dslipak）成功也跑 pdftotext，质量分
// 显著更低才切换；pdftotext 缺失（未安装/未配置 ABSORB_PDFTOTEXT）、下载
// 失败、提取为空 → 主路结果原样返回（零行为变化兜底）。
package extract

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
)

// popplerTimeout 单次 pdftotext 提取超时（347 页实测 ~3s，取 20 倍裕量）。
const popplerTimeout = 60 * time.Second

// pdftotextBin 解析 pdftotext 可执行文件路径：环境变量 ABSORB_PDFTOTEXT
// （显式路径，桌面端打包场景）优先，其次 PATH 查找。不可用返回空串。
func pdftotextBin() string {
	if p := strings.TrimSpace(os.Getenv("ABSORB_PDFTOTEXT")); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		return ""
	}
	if path, err := exec.LookPath("pdftotext"); err == nil {
		return path
	}
	return ""
}

// PopplerText 用 pdftotext -enc UTF-8 提取 PDF 字节文本，出参已过
// CollapsePDFSpacing（与主路提取同一后处理管线）。bin 为 pdftotext 路径。
// ctx 为父请求上下文：客户端断开/取消时立即终止 pdftotext 子进程，避免孤儿
// 进程空转至 popplerTimeout；ctx 取消优先于 60s 兜底超时。
func PopplerText(ctx context.Context, bin string, data []byte) (string, error) {
	if bin == "" || len(data) == 0 {
		return "", errors.New("poppler: pdftotext 未配置或输入为空")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	tmp, err := os.CreateTemp("", "absorb-poppler-*.pdf")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpPath)
	}()
	if _, err := tmp.Write(data); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	runCtx, cancel := context.WithTimeout(ctx, popplerTimeout)
	defer cancel()
	// 输出文件为 "-" = 写 stdout；stderr 的进度/告警不进入文本
	out, err := exec.CommandContext(runCtx, bin, "-enc", "UTF-8", tmpPath, "-").Output()
	if err != nil {
		return "", fmt.Errorf("poppler: pdftotext 执行失败: %w", err)
	}
	if len(out) == 0 {
		return "", errors.New("poppler: pdftotext 输出为空")
	}
	return CollapsePDFSpacing(string(out)), nil
}

// cjkSpacePairRe 中文逐字空格特征（"最 早 参 与"式 CID 字体伪空格，
// CollapsePDFSpacing 后的残留量是文本质量的主判别信号，Phase 0 实测
// 现有提取 1216 vs poppler 72，区分度 >15 倍）。
var cjkSpacePairRe = regexp.MustCompile(`[\p{Han}] [\p{Han}]`)

// textQualityScore 文本质量分（越低越好）：乱码率为主、CJK 逐字空格率为辅、
// 行内乱序率（P1 实验：dslipak 组装缺陷 +9.6pp 罕见 bigram 噪声，乱码/空格
// 两项对此完全失明——AgenticSE 未切到干净 poppler 文本的根因）再次之。
// 空文本重罚。
func textQualityScore(s string) float64 {
	rs := []rune(s)
	if len(rs) == 0 {
		return 1
	}
	return garbledRatio(s) + float64(len(cjkSpacePairRe.FindAllString(s, -1)))/float64(len(rs))/10 +
		rareBigramRate(s)/10
}

// 参照 bigram 集（P1 乱序对照实验产物）：从 3 本干净中文技术书（《人月神话》
// 《程序员的自我修养》《GO 专家编程》poppler 提取文本，约 46 万字）生成，
// 50,785 个去重汉字 bigram，固定测量基准。领域词漂移（干净书实测 14%~33%）
// 在同书双跑对比中抵消——择优只看相对差异，绝对率不设阈值。
//
//go:embed ref_bigrams.txt
var refBigramsData string

var (
	refBigramsOnce sync.Once
	refBigramsSet  map[string]bool
)

// refBigrams 懒加载参照集（首次调用约 50K map 插入，<10ms）。
func refBigrams() map[string]bool {
	refBigramsOnce.Do(func() {
		refBigramsSet = make(map[string]bool, len(refBigramsData)/4)
		for _, line := range strings.Split(refBigramsData, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				refBigramsSet[line] = true
			}
		}
		refBigramsData = "" // 释放内嵌原文（集合已建成）
	})
	return refBigramsSet
}

// minHanForRareTerm 行内乱序项的最小汉字量：bigram 统计对短文本噪声大，
// 低于此不参与打分（短 PDF 退回乱码+空格两项）。
const minHanForRareTerm = 500

// rareBigramRate 行内乱序率：文本内 Han 连续段相邻二字组中未被参照集覆盖的
// token 占比（0~1）。字符交错乱序（dslipak 同行带 span 交错组装）会制造
// 大量正常文本不存在的 bigram，乱序越重比率越高；阅读顺序正确的提取之间
// 差异极小（实测三模式 32.66%/32.67%/32.66%）。汉字量不足时返回 0。
func rareBigramRate(s string) float64 {
	ref := refBigrams()
	var (
		total, rare, han int
		run              []rune
	)
	score := func() {
		han += len(run)
		for i := 0; i+1 < len(run); i++ {
			total++
			if !ref[string(run[i:i+2])] {
				rare++
			}
		}
		run = run[:0]
	}
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			run = append(run, r)
		} else if len(run) > 0 {
			score()
		}
	}
	if len(run) > 0 {
		score()
	}
	if han < minHanForRareTerm || total == 0 {
		return 0
	}
	return float64(rare) / float64(total)
}

// pickBetterText 双引擎择优：主路乱码/空文本必选第二引擎；两路都可用时
// 质量分需有 ≥10% 的显著优势才切换（兼容优先——无谓换源会触发全量重建，
// 且主路字符级字号/坐标信息更全）。返回择优文本与是否发生了切换。
func pickBetterText(current string, currentBad bool, alt string) (string, bool) {
	if strings.TrimSpace(alt) == "" {
		return current, false
	}
	if currentBad || strings.TrimSpace(current) == "" {
		return alt, true
	}
	if textQualityScore(alt)*1.1 < textQualityScore(current) {
		return alt, true
	}
	return current, false
}
