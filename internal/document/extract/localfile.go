// localfile.go 本地 file:// URL → 本地路径转换（本地 PDF 提取接缝）。
// 浏览器侧的 file URL 形态：file:///D:/lj/电子书/k8s1.pdf（盘符前带 /，路径
// percent-encoding），url.Path 解析后已是解码形态 "/D:/lj/电子书/k8s1.pdf"。
package extract

import (
	"errors"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
)

// LocalFilePath file:// URL 转本地文件路径。
// Windows：file:///D:/x/y.pdf 的 url.Path = "/D:/x/y.pdf"（盘符前多一个 /），
// TrimPrefix 后 FromSlash 还原盘符路径；非 Windows：Path 即绝对路径（前导 /
// 是路径的一部分，不能剥）。仅接受三斜杠空 Host 形态——file://server/share
// 是 UNC/SMB 语义，一律拒绝。
func LocalFilePath(rawURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Scheme != "file" {
		return "", errors.New("非合法的 file:// URL")
	}
	if u.Host != "" {
		return "", errors.New("不支持 UNC/网络共享路径（file://server/share）")
	}
	if u.Path == "" {
		return "", errors.New("file URL 缺少路径")
	}
	p := u.Path
	if runtime.GOOS == "windows" {
		p = strings.TrimPrefix(p, "/")
	}
	p = filepath.FromSlash(p)
	// 浏览器标签页构造的 file URL 恒为绝对路径；file:///（Path="/"，Windows
	// TrimPrefix 后为空）与相对形态在此一并拒绝。
	if !filepath.IsAbs(p) {
		return "", errors.New("file URL 缺少本地绝对路径")
	}
	return p, nil
}
