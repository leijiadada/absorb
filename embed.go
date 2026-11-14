package main

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed all:frontend/dist
var frontendAssets embed.FS

//go:embed assets/default-config.yaml
var defaultConfigYAML []byte

//go:embed assets/appicon.png
var appIconPNG []byte

// resolveDataRoot 解析桌面端数据根并切换工作目录：
// exe 旁存在 data\ 目录 → 便携模式（exe 目录为根）；否则安装模式（%APPDATA%\absorb-agent）。
// chdir 后 LoadConfig/相对路径（./data、./config）全部落在数据根内。
func resolveDataRoot() string {
	exePath, err := os.Executable()
	if err != nil {
		exePath = "."
	}
	exeDir := filepath.Dir(exePath)
	if st, err := os.Stat(filepath.Join(exeDir, "data")); err == nil && st.IsDir() {
		// 便携模式同样保证 config 目录存在（db 落 exe 旁 data\，即标记目录本身）
		if err := os.MkdirAll(filepath.Join(exeDir, "config"), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "create config dir failed: %v\n", err)
			os.Exit(1)
		}
		_ = os.Chdir(exeDir)
		return exeDir
	}
	base, err := os.UserConfigDir()
	if err != nil {
		base = "."
	}
	root := filepath.Join(base, "absorb-agent")
	for _, d := range []string{"data", "config"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "create data dir failed: %v\n", err)
			os.Exit(1)
		}
	}
	if err := os.Chdir(root); err != nil {
		fmt.Fprintf(os.Stderr, "switch data dir failed: %v\n", err)
		os.Exit(1)
	}
	return root
}

// ensureDefaultConfig 首次启动生成默认 config.yaml（Key 置空，权限 0600）。
// GLM 系统 prompt 已内嵌进二进制（internal/chat/prompts/glm-system.md），不再落盘；
// 此处清理旧版本外部化时期残留的 config\glm-system.md（防明文暴露）。
func ensureDefaultConfig(cfgPath string) {
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		if err := os.WriteFile(cfgPath, defaultConfigYAML, 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "generate default config failed: %v\n", err)
			os.Exit(1)
		}
	}
	if legacy, err := os.Stat(filepath.Join("config", "glm-system.md")); err == nil && !legacy.IsDir() {
		_ = os.Remove(filepath.Join("config", "glm-system.md"))
	}
}
