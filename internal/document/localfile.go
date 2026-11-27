// localfile.go 本地 file:// URL 的白名单校验（本地 PDF 支持入口）。
// ValidatePublicURL 是"公网可达"语义（SSRF 防护，挡 file/ftp 等），本地文件
// 是另一类信任边界：路径来自浏览器标签页（用户自己打开的文件），桌面端以
// 当前用户身份读盘。收紧点：三斜杠空 Host（拒绝 file://server/share UNC/SMB
// 防止借浏览器标签探测网络共享）、仅 .pdf 后缀、文件存在且非目录。
package document

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"absorb-agent/internal/document/extract"
)

// validateFileURL file:// URL 校验，失败返回带原因的错误（prepare 层映射为
// StatusInvalidURL 展示给扩展端）。
func validateFileURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "file" {
		return errors.New("URL 格式无效")
	}
	if u.Host != "" {
		return errors.New("不支持网络共享路径（file://server/share），请使用本地盘路径")
	}
	path, err := extract.LocalFilePath(raw)
	if err != nil {
		return err
	}
	if !strings.EqualFold(filepath.Ext(path), ".pdf") {
		return errors.New("仅支持本地 .pdf 文件")
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("本地文件不可访问: %w", err)
	}
	if fi.IsDir() {
		return errors.New("路径是目录而非文件")
	}
	return nil
}
