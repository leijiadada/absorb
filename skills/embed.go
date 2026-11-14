// Package skills 内置技能资产：以 go:embed 打进二进制，
// 供 skills.root 未配置时解压到用户缓存目录兜底启用（见 internal/conf/skillconf.go）。
// 新增技能目录后在此追加 embed 模式即可随包分发。
package skills

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed mermaid visual-explanation contextual-learning-coach
var bundled embed.FS

// Materialize 将内嵌技能整树解压到 dest（逐文件覆盖写，随应用更新保持最新）。
func Materialize(dest string) error {
	return fs.WalkDir(bundled, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		out := filepath.Join(dest, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		data, err := bundled.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(out, data, 0o644)
	})
}
