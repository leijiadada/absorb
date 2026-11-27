//go:build windows

package upgrade

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

type windowsApplier struct{}

// newPlatformApplier Windows：经 PowerShell Start-Process 拉起安装程序，隐藏控制台窗口。
func newPlatformApplier() Applier { return windowsApplier{} }

// Install 经 PowerShell Start-Process 拉起安装程序（ShellExecute，自动触发 UAC 提升），
// 并等待启动器进程退出取回退出码：
//   - 拉起成功：start（非 -Wait）立即返回，PowerShell 退出码 0，本函数随即返回 nil；
//     安装器 [Setup] 的 InitializeSetup 会 taskkill 结束旧进程，postinstall 重启应用。
//   - 拉起失败（UAC 取消/文件关联错误等）：ShellExecuteEx 失败 → PowerShell 退出码
//     非 0 → 本函数返回 error → 前端复位“正在更新”并展示安装失败。
//
// 不用 cmd /c start：它对 ShellExecute 失败（尤其 UAC 取消）无可靠退出码，失败被
// 吞掉后调用方误以为安装已启动，前端永久卡在“正在更新”。
func (windowsApplier) Install(ctx context.Context, pkgPath string, _ Release) error {
	// 前置存在性检查：给出确定性错误而非 ShellExecute 泛化失败。
	if _, err := os.Stat(pkgPath); err != nil {
		return fmt.Errorf("installer not found: %w", err)
	}
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden",
		"-Command", fmt.Sprintf(`Start-Process -FilePath '%s'`, pkgPath))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("install launcher: %w", err)
	}
	return nil
}
