package upgrade

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
)

// 升级模块的哨兵错误。调用方用 errors.Is 判定，不匹配字符串。
var (
	// ErrNoUpdate 已是最新（或当前版本不低于 feed 版本）。
	ErrNoUpdate = errors.New("already up to date")
	// ErrThrottled 自动检查被节流跳过（间隔未到）。
	ErrThrottled = errors.New("update check skipped: interval not elapsed")
	// ErrDevBuild 当前为开发构建，无法与发布版比较。
	ErrDevBuild = errors.New("dev build: cannot compare with releases")
	// ErrInsecureURL feed / 安装包地址不是 https。
	ErrInsecureURL = errors.New("only https URLs are allowed")
	// ErrNoArtifact feed 中没有匹配当前平台的安装包。
	ErrNoArtifact = errors.New("no installable artifact for this platform")
	// ErrChecksum 安装包 SHA256 校验失败（文件已删除）。
	ErrChecksum = errors.New("artifact checksum mismatch")
	// ErrBadFeed feed 内容不合法（状态码、JSON、必填字段）。
	ErrBadFeed = errors.New("malformed update feed")
	// ErrBadVersion 版本串无法解析。
	ErrBadVersion = errors.New("malformed version")
	// ErrUnsupported 当前平台不支持自动安装。
	ErrUnsupported = errors.New("automatic install is not supported on this platform")
	// ErrNoFeedURL 未配置 feed 地址。
	ErrNoFeedURL = errors.New("feed url not configured")
	// ErrDisabled 升级检查被配置关闭（CheckAndNotify 静默跳过）。
	ErrDisabled = errors.New("update check disabled by configuration")
)

// Error 携带稳定机器码的用户可见错误。Error() 以 [CODE] 前缀输出，便于前端
// 跨语言映射为友好文案（Wails 只传字符串，无法 errors.Is）；Unwrap 仍返回底层
// 哨兵错误，故 Go 侧 errors.Is 判定不受影响。
type Error struct {
	Code   string // 稳定码，如 feed_unavailable / net_fail
	Status int    // 可选 HTTP 状态码（feed/下载非 200 时）
	Err    error  // 底层错误（哨兵或原始）
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("[%s] %s (status %d)", e.Code, e.Err.Error(), e.Status)
	}
	return fmt.Sprintf("[%s] %s", e.Code, e.Err.Error())
}

func (e *Error) Unwrap() error { return e.Err }

// WithCode 包装 err 为带稳定码的用户可见错误。
func WithCode(code string, err error) error {
	return &Error{Code: code, Err: err}
}

// WithStatus 包装 HTTP 相关错误并附带状态码（前端可据状态码细化文案）。
func WithStatus(code string, status int, err error) error {
	return &Error{Code: code, Status: status, Err: err}
}

// Code 取出 err 的机器码；非 *Error 返回空串。
func Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// Coded 把任意升级错误归一为带码 *Error：已带码原样返回；哨兵错误按表映射；
// 网络/超时类归为 net_fail；其余归为 unknown。供 Service 出口与通知统一调用。
func Coded(err error) error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return err
	}
	switch {
	case errors.Is(err, ErrBadFeed):
		return WithCode("bad_feed", err)
	case errors.Is(err, ErrNoFeedURL):
		return WithCode("no_feed_url", err)
	case errors.Is(err, ErrNoArtifact):
		return WithCode("no_artifact", err)
	case errors.Is(err, ErrChecksum):
		return WithCode("checksum", err)
	case errors.Is(err, ErrInsecureURL):
		return WithCode("insecure_url", err)
	case errors.Is(err, ErrUnsupported):
		return WithCode("unsupported", err)
	case errors.Is(err, ErrDevBuild):
		return WithCode("dev_build", err)
	case errors.Is(err, ErrNoUpdate):
		return WithCode("no_update", err)
	case errors.Is(err, ErrThrottled):
		return WithCode("throttled", err)
	case errors.Is(err, ErrDisabled):
		return WithCode("disabled", err)
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "download:"):
		return WithCode("download_fail", err)
	case strings.Contains(msg, "install") || strings.Contains(msg, "inno"):
		return WithCode("install_fail", err)
	}
	if isNetError(err) {
		return WithCode("net_fail", err)
	}
	return WithCode("unknown", err)
}

// isNetError 判定底层是否为网络类错误（连接拒绝/超时/DNS/TLS/上下文取消）。
func isNetError(err error) bool {
	if err == nil {
		return false
	}
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	m := strings.ToLower(err.Error())
	for _, k := range []string{"connection refused", "no such host", "timeout", "i/o timeout", "network is unreachable", "tls", "x509", "certificate"} {
		if strings.Contains(m, k) {
			return true
		}
	}
	return false
}
