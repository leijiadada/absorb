package conf

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"trpc.group/trpc-go/trpc-agent-go/agent/llmagent"
	"trpc.group/trpc-go/trpc-agent-go/skill"

	"absorb-agent/skills"
)

// bundledShellRel 捆绑 shell（MinGit）相对可执行文件/工作目录的位置。
// 当前技能仅知识加载、未装配 code executor，框架不会 spawn shell；
// 以下探测函数为恢复执行能力预留（框架届时固定 spawn `sh` 与 `bash`）。
var bundledShellRel = filepath.Join("runtime", "mingit", "usr", "bin")

// bundledPythonRel 捆绑嵌入式 Python 相对可执行文件/工作目录的位置。
// 当前未启用；技能脚本按 `python3 scripts/xxx.py` 调用而机器无 Python 时靠它兜底。
var bundledPythonRel = filepath.Join("runtime", "python")

// NewSkillsOptions 按配置构建技能相关 llmagent.Option。
// root 为空 → 依次兜底：exe 旁 skills/、工作目录 skills/、解压内嵌技能到用户缓存目录
// （见 skills.Materialize）；root 配置了但目录不存在 → 返回 (nil, warn, nil) 跳过技能；
// 警告列表供启动日志打印。注：当前技能仅知识加载，未装配 code executor（workspace_exec）。
func NewSkillsOptions(root string) ([]llmagent.Option, []string, error) {
	resolved, warns := resolveSkillsRoot(strings.TrimSpace(root), runtimeBases(), materializeBundledSkills)
	if resolved == "" {
		return nil, warns, nil // 未配置且兜底全部落空：不启用
	}
	st, err := os.Stat(resolved)
	if err != nil || !st.IsDir() {
		return nil, append(warns, "skills 目录不存在，跳过技能启用: "+resolved), nil
	}
	repo, err := skill.NewFSRepository(resolved)
	if err != nil {
		return nil, nil, fmt.Errorf("skills repo: %w", err)
	}
	return []llmagent.Option{llmagent.WithSkills(repo)}, warns, nil
}

// materializeBundledSkills 把编译期内嵌技能解压到用户缓存目录 absorb/skills，
// 返回解压根目录。变量形式便于单测注入。
var materializeBundledSkills = func() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dest := filepath.Join(base, "absorb", "skills")
	if err := skills.Materialize(dest); err != nil {
		return "", err
	}
	return dest, nil
}

// resolveSkillsRoot 技能根目录解析（纯逻辑，便于单测）：
// 显式配置优先；为空时按 bases 顺序找现成的 skills/ 目录，找不到再解压内嵌兜底。
func resolveSkillsRoot(root string, bases []string, bundledFn func() (string, error)) (string, []string) {
	if root != "" {
		return root, nil
	}
	for _, base := range bases {
		p := filepath.Join(base, "skills")
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return p, nil
		}
	}
	p, err := bundledFn()
	if err != nil {
		return "", []string{"内置技能解压失败，技能未启用: " + err.Error()}
	}
	return p, nil
}

// runtimeBases 返回捆绑运行时候选的 base 目录列表：
// 可执行文件目录（正式分发，exe 旁）与进程工作目录（开发调试，go run）两种形态。
func runtimeBases() []string {
	bases := []string{}
	if exe, err := os.Executable(); err == nil {
		bases = append(bases, filepath.Dir(exe))
	}
	if wd, err := os.Getwd(); err == nil {
		bases = append(bases, wd)
	}
	return bases
}

// fiExists 判断路径存在且为普通文件（非目录）。
func fiExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

// firstBundledShellDir 从候选 base 目录中找出第一个含捆绑 sh 与 bash 的目录，便于单元测试注入。
// 两者都必须存在才算完整（框架对 sh、bash 都会 spawn）。
func firstBundledShellDir(bases ...string) string {
	for _, base := range bases {
		dir := filepath.Join(base, bundledShellRel)
		shOK := fiExists(filepath.Join(dir, "sh.exe"))
		bashOK := fiExists(filepath.Join(dir, "bash.exe"))
		if shOK && bashOK {
			return dir
		}
	}
	return ""
}

// systemShellOK 判断系统是否有真正可用的 bash 与 sh。
// 框架固定 spawn `sh`（workspace_exec 命令）与 `bash`（工作区元数据提交/技能运行），
// 两者都必须可用；任一缺失或解析到坏桩（WSL 启动器、Store 别名）都判定不可用。
func systemShellOK() bool {
	return shellNameOK("bash") && shellNameOK("sh")
}

// shellNameOK 判断指定 shell 名能否解析到真实可用的解释器。
func shellNameOK(name string) bool {
	p, err := exec.LookPath(name)
	if err != nil {
		return false
	}
	return !isBrokenShellStub(p)
}

// isBrokenShellStub 识别两类"能 LookPath 到但不是真解释器"的 Windows 桩：
//   - C:\Windows\System32\bash.exe：WSL 启动器，未安装默认发行版时任何调用必失败（exit 1）
//   - %LOCALAPPDATA%\Microsoft\WindowsApps\*.exe：Microsoft Store 的 App Execution Alias 桩
func isBrokenShellStub(p string) bool {
	low := strings.ToLower(p)
	if strings.Contains(low, "system32") && strings.HasSuffix(low, "bash.exe") {
		return true
	}
	return strings.Contains(low, "windowsapps")
}

// firstBundledPythonDir 从候选 base 目录中找出第一个含捆绑 python 的目录，便于单元测试注入。
// 命中返回 runtime/python 目录（需 prepend 到 PATH 供 `python3`/`python` 解析）。
func firstBundledPythonDir(bases ...string) string {
	for _, base := range bases {
		dir := filepath.Join(base, bundledPythonRel)
		if fiExists(filepath.Join(dir, "python.exe")) {
			return dir
		}
	}
	return ""
}

// systemPythonOK 判断系统是否有真正可用的 python3/python 解释器。
// Windows 上 `python3` 常是 Microsoft Store 的 App Execution Alias 桩
// （位于 %LOCALAPPDATA%\Microsoft\WindowsApps），能 LookPath 到但并非真解释器，须排除。
func systemPythonOK() bool {
	for _, name := range []string{"python3", "python"} {
		p, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if strings.Contains(strings.ToLower(p), "windowsapps") {
			continue
		}
		return true
	}
	return false
}
