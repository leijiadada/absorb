package upgrade

import (
	"context"
	"io"
)

// Applier 把已校验的安装包装上。默认实现按平台分发（applier_windows.go /
// applier_other.go）；测试注入假实现即可覆盖 Apply 全流程，无需真实安装。
type Applier interface {
	// Install 启动安装流程。实现方负责让安装器与当前进程解耦
	// （安装器自行结束旧进程并在装完后重启应用）。
	Install(ctx context.Context, pkgPath string, rel Release) error
}

// NewApplier 返回当前平台的默认 Applier（out 预留给日志输出，当前实现未使用）。
func NewApplier(_ io.Writer) Applier { return newPlatformApplier() }
