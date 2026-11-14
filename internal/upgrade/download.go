package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ProgressFunc 下载进度（done/total 字节；total <= 0 表示长度未知）。
type ProgressFunc func(done, total int64)

// Downloader 下载安装包并校验 SHA256。校验不通过立即删除临时文件——
// 绝不把未校验产物交给 Applier。
type Downloader struct {
	Client   *http.Client // nil = 默认 10 分钟超时客户端
	Dir      string       // 暂存目录（调用方负责可配置；默认 %TEMP%\absorb-agent-upgrade）
	Progress ProgressFunc
}

func (d *Downloader) client() *http.Client {
	if d.Client != nil {
		return d.Client
	}
	return &http.Client{Timeout: 10 * time.Minute}
}

// Fetch 下载 a 到 Dir 并校验，返回最终文件路径。
func (d *Downloader) Fetch(ctx context.Context, a Artifact) (string, error) {
	if !isHTTPS(a.URL) {
		return "", fmt.Errorf("%w: %s", ErrInsecureURL, a.URL)
	}
	if d.Dir == "" {
		return "", fmt.Errorf("download dir not configured")
	}
	if err := os.MkdirAll(d.Dir, 0o700); err != nil {
		return "", fmt.Errorf("create download dir: %w", err)
	}
	name := filepath.Base(strings.TrimSpace(a.Name))
	// Base 已挡掉路径穿越（"../x.exe" → "x.exe"）；这里再拒掉空/"."/".." 这类
	// 会落到目录本身或父目录的名字，避免 Rename 目标变成目录时报难以理解的错误。
	if name == "" || name == "." || name == ".." || name == string(filepath.Separator) {
		return "", fmt.Errorf("%w: bad artifact name %q", ErrBadFeed, a.Name)
	}
	part := filepath.Join(d.Dir, name+".part")
	final := filepath.Join(d.Dir, name)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return "", fmt.Errorf("download: %v", err)
	}
	resp, err := d.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download: unexpected status %s", resp.Status)
	}

	f, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	hasher := sha256.New()
	var done int64
	buf := make([]byte, 128*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				_ = f.Close()
				_ = os.Remove(part)
				return "", fmt.Errorf("write temp file: %w", werr)
			}
			_, _ = hasher.Write(buf[:n])
			done += int64(n)
			if d.Progress != nil {
				d.Progress(done, resp.ContentLength)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			_ = f.Close()
			_ = os.Remove(part)
			return "", fmt.Errorf("download: %w", rerr)
		}
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(part)
		return "", fmt.Errorf("close temp file: %w", err)
	}

	sum := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(sum, a.SHA256) {
		_ = os.Remove(part)
		return "", fmt.Errorf("%w: got %s, want %s", ErrChecksum, sum, a.SHA256)
	}
	if err := os.Rename(part, final); err != nil {
		_ = os.Remove(part)
		return "", fmt.Errorf("move artifact into place: %w", err)
	}
	return final, nil
}
