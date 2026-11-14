package conf

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestFirstBundledShellDir 验证捆绑 shell 探测：命中/缺失/顺序优先。
func TestFirstBundledShellDir(t *testing.T) {
	base := t.TempDir()
	// 构造 <base>/runtime/mingit/usr/bin/sh.exe + bash.exe
	dir := filepath.Join(base, bundledShellRel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sh.exe"), []byte("test"), 0o644); err != nil {
		t.Fatalf("write sh.exe: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bash.exe"), []byte("test"), 0o644); err != nil {
		t.Fatalf("write bash.exe: %v", err)
	}

	if got := firstBundledShellDir(base); got != dir {
		t.Fatalf("期望命中 %s，实际 %q", dir, got)
	}
	if got := firstBundledShellDir(t.TempDir()); got != "" {
		t.Fatalf("空目录应返回空，实际 %q", got)
	}
	// 只缺 bash.exe 视为不完整
	missingBash := t.TempDir()
	d2 := filepath.Join(missingBash, bundledShellRel)
	if err := os.MkdirAll(d2, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(d2, "sh.exe"), []byte("test"), 0o644); err != nil {
		t.Fatalf("write sh.exe: %v", err)
	}
	if got := firstBundledShellDir(missingBash); got != "" {
		t.Fatalf("缺 bash.exe 应返回空，实际 %q", got)
	}
	// 第一个 base 命中后，第二个不参与
	if got := firstBundledShellDir(base, t.TempDir()); got != dir {
		t.Fatalf("顺序优先命中失败: %q", got)
	}
}

// TestIsBrokenShellStub 验证坏桩识别：WSL 启动器 / Store 别名视为坏桩，Git Bash 真实路径不误伤。
func TestIsBrokenShellStub(t *testing.T) {
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"WSL 启动器", `C:\Windows\System32\bash.exe`, true},
		{"WSL 启动器-大小写不敏感", `C:\WINDOWS\SYSTEM32\BASH.EXE`, true},
		{"Store 别名 bash", `C:\Users\a\AppData\Local\Microsoft\WindowsApps\bash.exe`, true},
		{"Store 别名 python3", `C:\Users\a\AppData\Local\Microsoft\WindowsApps\python3.exe`, true},
		{"Git Bash 真实解释器", `D:\Git\usr\bin\bash.exe`, false},
		{"Git Bash sh", `D:\Git\usr\bin\sh.exe`, false},
		{"捆绑 MinGit", `D:\runtime\mingit\usr\bin\bash.exe`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isBrokenShellStub(c.path); got != c.want {
				t.Fatalf("isBrokenShellStub(%q) = %v，期望 %v", c.path, got, c.want)
			}
		})
	}
}

// TestSystemShellOKDetectsWslLauncher 模拟用户机器的真实情况：
// System32\bash.exe（WSL 启动器）在 PATH 首位，即使装了 Git Bash 也会被它顶掉 → 判定 shell 不可用。
func TestSystemShellOKDetectsWslLauncher(t *testing.T) {
	sys32 := filepath.Join(t.TempDir(), "System32")
	if err := os.MkdirAll(sys32, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sys32, "bash.exe"), []byte("wsl launcher"), 0o644); err != nil {
		t.Fatalf("write bash.exe: %v", err)
	}
	gitBash := t.TempDir()
	if err := os.WriteFile(filepath.Join(gitBash, "bash.exe"), []byte("real bash"), 0o644); err != nil {
		t.Fatalf("write bash.exe: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gitBash, "sh.exe"), []byte("real sh"), 0o644); err != nil {
		t.Fatalf("write sh.exe: %v", err)
	}
	origPATH := os.Getenv("PATH")
	t.Cleanup(func() { os.Setenv("PATH", origPATH) })
	os.Setenv("PATH", sys32+string(os.PathListSeparator)+gitBash)
	if systemShellOK() {
		t.Fatal("System32 WSL 启动器占位时不应判定 shell 可用（即使 Git Bash 在后面）")
	}
	// 去掉 WSL 启动器后，Git Bash 可正常解析 → 判定可用
	os.Setenv("PATH", gitBash)
	if !systemShellOK() {
		t.Fatal("仅 Git Bash 时应判定可用")
	}
}

// TestFirstBundledPythonDir 验证捆绑 python 探测：命中/缺失/顺序优先。
func TestFirstBundledPythonDir(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, bundledPythonRel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "python.exe"), []byte("test"), 0o644); err != nil {
		t.Fatalf("write python.exe: %v", err)
	}

	if got := firstBundledPythonDir(base); got != dir {
		t.Fatalf("期望命中 %s，实际 %q", dir, got)
	}
	if got := firstBundledPythonDir(t.TempDir()); got != "" {
		t.Fatalf("空目录应返回空，实际 %q", got)
	}
}

// TestSystemPythonOKSkipsStoreStub 验证 Windows Store 的 python3 桩（WindowsApps 目录）不被当作真解释器。
func TestSystemPythonOKSkipsStoreStub(t *testing.T) {
	stub := filepath.Join(t.TempDir(), "WindowsApps") // 模拟 %LOCALAPPDATA%\Microsoft\WindowsApps
	if err := os.MkdirAll(stub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stub, "python3.exe"), []byte("stub"), 0o644); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	origPATH := os.Getenv("PATH")
	t.Cleanup(func() { os.Setenv("PATH", origPATH) })
	os.Setenv("PATH", stub)
	if systemPythonOK() {
		t.Fatal("仅存在 Store 桩 python3 时应判定为不可用")
	}
}

// TestBundledShellEndToEnd 模拟未装 Git Bash 的机器：PATH 注入后，
// LookPath("sh")/"bash" 与真实 spawn 都必须解析到捆绑 MinGit。
// bash 是框架元数据提交/技能运行用的解释器（本机真实故障点），sh 是 workspace_exec 用的。
// 依赖仓库 runtime/mingit 存在，否则跳过（开发环境可装 Git Bash 兜底）。
func TestBundledShellEndToEnd(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller 失败")
	}
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(file))) // internal/conf/ → 仓库根
	dir := firstBundledShellDir(repoRoot)
	if dir == "" {
		t.Skip("未找到捆绑 MinGit（runtime/mingit），跳过端到端验证")
	}

	// 清空 PATH 中除系统目录外的条目，模拟无 Git Bash 环境
	var sysDirs []string
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if strings.HasPrefix(strings.ToLower(p), filepath.VolumeName(p)) &&
			strings.Contains(strings.ToLower(p), "system32") {
			sysDirs = append(sysDirs, p)
		}
	}
	origPATH := os.Getenv("PATH")
	t.Cleanup(func() { os.Setenv("PATH", origPATH) })
	os.Setenv("PATH", strings.Join(sysDirs, string(os.PathListSeparator)))
	if _, err := exec.LookPath("sh"); err == nil {
		t.Skip("本机系统 sh 仍在 PATH 中，跳过（无法模拟无 Git Bash 环境）")
	}

	// 与 NewSkillsOptions 相同的注入逻辑
	os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, name := range []string{"sh", "bash"} {
		bin, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("注入后 LookPath(%s) 失败: %v", name, err)
		}
		if !strings.HasPrefix(strings.ToLower(bin), strings.ToLower(dir)) {
			t.Fatalf("%s 应解析到捆绑目录 %s，实际 %s", name, dir, bin)
		}
	}
	out, err := exec.Command("sh", "-lc", "echo BUNDLED_SH_OK").Output()
	if err != nil {
		t.Fatalf("spawn 捆绑 sh 失败: %v", err)
	}
	if strings.TrimSpace(string(out)) != "BUNDLED_SH_OK" {
		t.Fatalf("sh 异常输出: %q", out)
	}
	// bash 链路：模拟框架元数据提交的 spawn 方式（stager/skill run 均用 bash -lc）
	bout, err := exec.Command("bash", "-lc", "echo BUNDLED_BASH_OK").Output()
	if err != nil {
		t.Fatalf("spawn 捆绑 bash 失败: %v", err)
	}
	if strings.TrimSpace(string(bout)) != "BUNDLED_BASH_OK" {
		t.Fatalf("bash 异常输出: %q", bout)
	}
}
