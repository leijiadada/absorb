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

// newPlatformApplier Windows：经 cmd /c start 拉起安装程序，隐藏控制台窗口。
func newPlatformApplier() Applier { return windowsApplier{} }

// Install 启动安装程序后立即返回，不等待：
//   - 安装器 [Setup] 的 InitializeSetup 会 taskkill 结束旧进程，避免文件占用；
//   - [Run] 的 postinstall（非静默）在装完后自动重启应用。
//
// 因此本进程无需等待，也不负责重启。
func (windowsApplier) Install(ctx context.Context, pkgPath string, _ Release) error {
	// 前置存在性检查：cmd /c start 对不存在的文件会弹系统级 GUI 错误对话框
	// （"Windows 找不到文件"）并阻塞等人点确定，必须先拦下确定性报错。
	if _, err := os.Stat(pkgPath); err != nil {
		return fmt.Errorf("installer not found: %w", err)
	}
	cmd := exec.CommandContext(ctx, "cmd", "/c", "start", "", pkgPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("launch installer: %w", err)
	}
	// 不 Wait：安装器是独立进程，本进程随即退出。
	_ = cmd.Process.Release()
	return nil
}
