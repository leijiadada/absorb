package skills

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMaterialize 内嵌技能整树解压：目录创建、文件落盘、重复调用覆盖不报错。
func TestMaterialize(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "skills")
	if err := Materialize(dest); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	for _, want := range []string{
		filepath.Join("mermaid", "SKILL.md"),
		filepath.Join("visual-explanation", "SKILL.md"),
		filepath.Join("contextual-learning-coach", "SKILL.md"),
	} {
		data, err := os.ReadFile(filepath.Join(dest, want))
		if err != nil {
			t.Fatalf("缺少 %s: %v", want, err)
		}
		if len(data) == 0 {
			t.Fatalf("%s 为空", want)
		}
	}
	// 二次解压（模拟应用升级）覆盖写不报错
	if err := Materialize(dest); err != nil {
		t.Fatalf("重复 Materialize: %v", err)
	}
}
