package conf

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveSkillsRoot 技能根解析：显式配置优先、bases 顺序兜底、内嵌解压兜底、全落空告警。
func TestResolveSkillsRoot(t *testing.T) {
	bundledOK := func() (string, error) { return "bundled-root", nil }
	bundledErr := func() (string, error) { return "", os.ErrPermission }

	// 显式配置优先，不碰兜底（root 由调用方 TrimSpace 后传入）
	got, warns := resolveSkillsRoot("D:\\custom\\skills", nil, bundledErr)
	if got != "D:\\custom\\skills" || warns != nil {
		t.Fatalf("显式配置应原样返回（TrimSpace 后），got %q warns %v", got, warns)
	}

	// bases 顺序命中第一个含 skills/ 的目录
	base1, base2 := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(base2, "skills"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	got, warns = resolveSkillsRoot("", []string{base1, base2}, bundledErr)
	if got != filepath.Join(base2, "skills") || warns != nil {
		t.Fatalf("应命中 base2 的 skills 目录，got %q warns %v", got, warns)
	}

	// bases 全落空 → 解压内嵌兜底
	got, warns = resolveSkillsRoot("", []string{base1}, bundledOK)
	if got != "bundled-root" || warns != nil {
		t.Fatalf("应走内嵌兜底，got %q warns %v", got, warns)
	}

	// 兜底也失败 → 返回空 + 告警（技能不启用但不致命）
	got, warns = resolveSkillsRoot("", []string{base1}, bundledErr)
	if got != "" || len(warns) != 1 {
		t.Fatalf("兜底失败应返回空+告警，got %q warns %v", got, warns)
	}
}

// TestNewSkillsOptionsDisabledWhenRootMissing 显式配置了不存在目录 → 跳过并告警（保持原行为）。
func TestNewSkillsOptionsDisabledWhenRootMissing(t *testing.T) {
	opts, warns, err := NewSkillsOptions(filepath.Join(t.TempDir(), "nope"))
	if err != nil || opts != nil || len(warns) != 1 {
		t.Fatalf("目录不存在应返回 nil opts + 1 条告警，opts=%v warns=%v err=%v", opts, warns, err)
	}
}
