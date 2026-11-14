package extension

import (
	"archive/zip"
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

//go:embed extension_bundle.zip
var bundle []byte

// ExportResult 通用操作结果。
type ExportResult struct {
	OK    bool   `json:"ok"`
	Path  string `json:"path,omitempty"`
	Error string `json:"error,omitempty"`
}

// browserCheck 浏览器进程检测结果缓存（tasklist 开销大，5s 轮询不重复跑）。
var (
	browserRunningCache atomic.Bool
	browserCheckedAt    atomic.Int64
)

// BrowserRunning 检测 Edge/Chrome 是否有进程在运行，结果缓存 10 秒。
// tasklist 过滤无匹配时退出码仍为 0，必须检查输出内容。
func BrowserRunning() bool {
	now := time.Now().Unix()
	if now-browserCheckedAt.Load() < 10 {
		return browserRunningCache.Load()
	}
	running := false
	for _, img := range []string{"msedge.exe", "chrome.exe"} {
		cmd := exec.Command("tasklist", "/FI", "IMAGENAME eq "+img, "/NH")
		// GUI 进程启动控制台程序必须隐藏窗口，否则状态轮询会反复闪黑窗
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
		out, err := cmd.CombinedOutput()
		if err == nil && strings.Contains(strings.ToLower(string(out)), img) {
			running = true
			break
		}
	}
	browserRunningCache.Store(running)
	browserCheckedAt.Store(now)
	return running
}

// RunningProbe 浏览器进程探测入口（可替换注入，测试 stub 用）。
var RunningProbe = BrowserRunning

// State 扩展连接四态判定：Native 管道在=connected（秒级感知）；曾连接且全部
// 断开=disconnected/browser-closed（按浏览器进程探测区分）；从未活跃=never。
// lastSeen 为最近一次 Native 事件时刻，随状态值一并下发 UI 展示。
func State(lastSeen, nativeConns, disconnectAt int64) string {
	if nativeConns > 0 {
		return "connected"
	}
	// 曾有 Native 管道且已全部断开：保持断开语义直到重连，不随时间"恢复"为已连接
	if disconnectAt > 0 {
		if RunningProbe() {
			return "disconnected"
		}
		return "browser-closed"
	}
	return "never"
}

// Export 释放内嵌扩展到数据目录的 absorb-extension 文件夹（自动解压），
// openExplorer 为 true 时打开资源管理器定位该文件夹（仅 GUI 模式），供拖拽安装使用。
func Export(dataRoot string, openExplorer bool) ExportResult {
	base := filepath.Join(dataRoot, "extension")
	target := filepath.Join(base, "absorb-extension")
	// 先清空再解压，保证文件夹内容与当前 exe 内嵌版本一致
	if err := os.RemoveAll(target); err != nil {
		return ExportResult{Error: err.Error()}
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return ExportResult{Error: err.Error()}
	}
	if err := unzip(bundle, target); err != nil {
		return ExportResult{Error: err.Error()}
	}
	_ = os.Remove(filepath.Join(base, "absorb-extension.zip")) // 清理临时 zip
	if openExplorer {
		_ = exec.Command("explorer", "/select,"+target).Start()
	}
	return ExportResult{OK: true, Path: target}
}

// unzip 将 zip 字节流解压到 dir（标准库实现，零依赖）。
func unzip(data []byte, dir string) error {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	for _, f := range zr.File {
		name := filepath.Clean(f.Name)
		if strings.Contains(name, "..") {
			return fmt.Errorf("非法压缩路径: %s", f.Name)
		}
		dest := filepath.Join(dir, name)
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.Create(dest)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		rc.Close()
		out.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// browserPaths 浏览器可执行文件候选路径（按优先级排列）。
var browserPaths = []string{
	`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
	`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
	`C:\Program Files\Google\Chrome\Application\chrome.exe`,
	`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
}

// OpenExtensionsPage 启动本机 Edge/Chrome（不带 URL）。
// edge:// 等特权协议页禁止通过命令行 URL 打开（浏览器静默忽略），无法直达扩展管理页，
// 由 UI 引导用户在地址栏访问（前端负责复制地址）。
func OpenExtensionsPage() ExportResult {
	for _, p := range browserPaths {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if err := exec.Command(p).Start(); err == nil {
			return ExportResult{OK: true}
		}
	}
	return ExportResult{Error: "未找到 Edge 或 Chrome，请手动打开浏览器"}
}
