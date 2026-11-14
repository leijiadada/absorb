package errs

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestErrorsByCodeCoversAllCodes 保证前端 i18n.js 的 errorsByCode 表覆盖后端
// errs.Code 的全部稳定码，防止新增错误码时漏加前端文案（导致对应错误回退英文）。
//
// 单源设计：后端 errs.go 的 Code 字符串字面量为权威集合（常量声明 + defaultMessages
// 键均为码值）；前端 JS 为待校验镜像。测试直接读取两个源文件做集合包含校验，
// 任一语言块缺码、或前后端码表不一致都会失败。
func TestErrorsByCodeCoversAllCodes(t *testing.T) {
	// 1) 收集后端所有稳定码：errs.go 中的 "model.xxx" / "agent.xxx" 字面量
	src, err := os.ReadFile("errs.go")
	if err != nil {
		t.Fatalf("read errs.go: %v", err)
	}
	codeRe := regexp.MustCompile(`"(model|agent)\.[a-z_]+"`)
	backend := map[string]bool{}
	for _, m := range codeRe.FindAllString(string(src), -1) {
		backend[strings.Trim(m, `"`)] = true
	}
	if len(backend) == 0 {
		t.Fatal("未从 errs.go 解析到任何稳定码，检查解析逻辑")
	}

	// 2) 收集前端各语言 errorsByCode 块的 key（JS 用单引号）
	jsPath := "../../extension/sidepanel/js/i18n.js"
	js, err := os.ReadFile(jsPath)
	if err != nil {
		t.Fatalf("read %s: %v（前端 i18n 路径可能变动）", jsPath, err)
	}
	blockRe := regexp.MustCompile(`errorsByCode:\s*\{([\s\S]*?)\},`)
	keyRe := regexp.MustCompile(`'(model|agent)\.[a-z_]+'`)
	var blockSets []map[string]bool
	for _, b := range blockRe.FindAllStringSubmatch(string(js), -1) {
		s := map[string]bool{}
		for _, k := range keyRe.FindAllString(b[1], -1) {
			s[strings.Trim(k, `'`)] = true
		}
		blockSets = append(blockSets, s)
	}
	if len(blockSets) == 0 {
		t.Fatal("未从 i18n.js 解析到 errorsByCode 块，检查解析逻辑")
	}

	// 3) 每个后端码至少在一个语言块中出现（中文用户绝不看到英文回退）
	union := map[string]bool{}
	for _, s := range blockSets {
		for k := range s {
			union[k] = true
		}
	}
	var missing []string
	for c := range backend {
		if !union[c] {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("前端 errorsByCode 缺失以下后端稳定码（会导致对应错误回退英文）: %v", missing)
	}

	// 4) 各语言块之间保持一致（防止某语言漏加/多加）
	if len(blockSets) > 1 {
		first := blockSets[0]
		for i := 1; i < len(blockSets); i++ {
			for k := range first {
				if !blockSets[i][k] {
					t.Errorf("语言块 %d 缺少码 %s（与块0不一致）", i, k)
				}
			}
			for k := range blockSets[i] {
				if !first[k] {
					t.Errorf("语言块 %d 多出未登记码 %s（与块0不一致）", i, k)
				}
			}
		}
	}
}
