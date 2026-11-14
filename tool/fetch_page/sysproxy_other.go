//go:build !windows

package fetch

// SystemProxySnapshot 非 Windows 平台不支持 WinINET 注册表，恒为未启用。
func SystemProxySnapshot() (server, override string, enabled bool) {
	return "", "", false
}
