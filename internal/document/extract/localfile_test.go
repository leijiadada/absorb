package extract

// pdftotextBin 捆绑运行时探测的单测：runtime/poppler 目录形态、候选顺序、
// 环境变量清场。PATH 兜底分支依赖测试机环境，不做断言（仅保证不 panic）。

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPdftotextBinBundledLookup(t *testing.T) {
	t.Setenv("ABSORB_PDFTOTEXT", "") // 清除环境变量，专测捆绑探测
	name := "pdftotext"
	if runtime.GOOS == "windows" {
		name = "pdftotext.exe"
	}
	missDir := t.TempDir()
	hitDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(hitDir, "runtime", "poppler"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(hitDir, "runtime", "poppler", name)
	if err := os.WriteFile(bin, []byte(""), 0o755); err != nil {
		t.Fatal(err)
	}
	// 命中顺序：exe 目录 → 工作目录；第一个候选落空、第二个命中
	if got := pdftotextBinIn([]string{missDir, hitDir}); got != bin {
		t.Fatalf("got %q want %q", got, bin)
	}
	if got := pdftotextBinIn([]string{missDir}); got != "" && got == bin {
		t.Fatalf("落空候选不应命中捆绑路径: %q", got)
	}
	_ = pdftotextBinIn([]string{t.TempDir()}) // PATH 兜底分支仅执行不断言
}
