//go:build windows

package fetch

import (
	"net/url"
	"strings"

	"golang.org/x/sys/windows/registry"
)

func init() {
	systemProxyLookup = windowsSystemProxy
}

// windowsSystemProxy 读 WinINET 注册表（Clash/v2rayN 等代理工具"系统代理"模式写入的值），
// 实时读取保证系统代理开关后下一次请求即跟随。未启用或未配置时回退环境变量。
func windowsSystemProxy(u *url.URL) (*url.URL, bool) {
	server, override, ok := systemProxySnapshot()
	if !ok {
		return nil, false
	}
	p := parseProxyServer(server)
	if p == "" {
		return nil, false
	}
	if hostBypassed(u.Host, override) {
		return nil, true
	}
	pu, err := url.Parse(p)
	if err != nil {
		return nil, false
	}
	return pu, true
}

// systemProxySnapshot 读取系统代理原始配置：ProxyServer、ProxyOverride、是否启用。
// 仅覆盖静态代理模式；PAC 脚本模式（AutoConfigURL）不解析，表现为未启用。
func systemProxySnapshot() (server, override string, enabled bool) {
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if err != nil {
		return "", "", false
	}
	defer k.Close()
	enable, _, err := k.GetIntegerValue("ProxyEnable")
	if err != nil || enable != 1 {
		return "", "", false
	}
	server, _, err = k.GetStringValue("ProxyServer")
	if err != nil || strings.TrimSpace(server) == "" {
		return "", "", false
	}
	override, _, _ = k.GetStringValue("ProxyOverride")
	return server, override, true
}

// SystemProxySnapshot 导出系统代理快照（桌面设置页展示"跟随系统"效果用）。
func SystemProxySnapshot() (server, override string, enabled bool) {
	return systemProxySnapshot()
}
