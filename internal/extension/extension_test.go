package extension

import (
	"os"
	"path/filepath"
	"testing"
)

// TestExport 释放内嵌扩展：manifest 必须位于文件夹顶层，"加载解压缩的扩展"指向该文件夹；二次导出幂等。
func TestExport(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r := Export(dir, false)
	if !r.OK || r.Path == "" {
		t.Fatalf("导出失败: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(r.Path, "manifest.json")); err != nil {
		t.Fatalf("manifest.json 未释放到顶层: %v", err)
	}
	for _, sub := range []string{filepath.Join("background", "background.js"), "grab.js", filepath.Join("sidepanel", "sidepanel.html")} {
		if _, err := os.Stat(filepath.Join(r.Path, sub)); err != nil {
			t.Fatalf("缺少 %s: %v", sub, err)
		}
	}
	// 二次导出应幂等（RemoveAll 后重建）
	if r2 := Export(dir, false); !r2.OK || r2.Path != r.Path {
		t.Fatalf("重复导出不幂等: %+v vs %+v", r2, r)
	}
}
