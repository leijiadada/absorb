//go:build !windows

package upgrade

import (
	"context"
	"fmt"
)

type stubApplier struct{}

// newPlatformApplier 非 Windows 平台：安装包只产出 Windows 版，无自动安装实现。
func newPlatformApplier() Applier { return stubApplier{} }

func (stubApplier) Install(context.Context, string, Release) error {
	return fmt.Errorf("%w: windows only", ErrUnsupported)
}
