package fetch

import (
	"net"
	"net/url"
	"strings"
)

// 系统代理探测钩子：由平台文件注入实现（Windows 读 WinINET 注册表），测试可替换。
// 约定：(u, true) 使用 u 作为代理；(nil, true) 命中例外地址直连；(nil, false) 未检测到，
// 由调用方回退环境变量。
var systemProxyLookup = defaultSystemProxyLookup

func defaultSystemProxyLookup(*url.URL) (*url.URL, bool) {
	return nil, false
}

// parseProxyServer 解析 WinINET ProxyServer 值为单个代理 URL。
// 两种形态："host:port"（全协议共用）与 "http=a;https=b;ftp=c;socks=d"（按协议拆分，
// 优先 https，其次 http，最后 socks；https/http 的代理协议仍是 HTTP，socks 映射为 socks5://）。
func parseProxyServer(server string) string {
	server = strings.TrimSpace(server)
	if server == "" {
		return ""
	}
	if !strings.Contains(server, "=") {
		if strings.Contains(server, "://") {
			return server
		}
		return "http://" + server
	}
	var httpsAddr, httpAddr, socksAddr string
	for _, part := range strings.Split(server, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || v == "" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "https":
			httpsAddr = v
		case "http":
			httpAddr = v
		case "socks":
			socksAddr = v
		}
	}
	pairs := []struct{ scheme, addr string }{
		{"http://", httpsAddr},
		{"http://", httpAddr},
		{"socks5://", socksAddr},
	}
	for _, p := range pairs {
		if p.addr == "" {
			continue
		}
		if strings.Contains(p.addr, "://") {
			return p.addr
		}
		return p.scheme + p.addr
	}
	return ""
}

// hostBypassed 判断请求 host 是否命中 ProxyOverride 例外地址（分号分隔，如
// "localhost;127.*;192.168.*;<local>"），命中则不走系统代理直连。
// "<local>" 表示不含点号的主机名（局域网内网机器名）。
func hostBypassed(host, override string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "" {
		return false
	}
	for _, pat := range strings.Split(override, ";") {
		pat = strings.ToLower(strings.TrimSpace(pat))
		if pat == "" {
			continue
		}
		if pat == "<local>" {
			if !strings.Contains(host, ".") {
				return true
			}
			continue
		}
		if matchGlob(host, strings.TrimPrefix(pat, ".")) {
			return true
		}
	}
	return false
}

// matchGlob 通配符匹配：* 匹配任意子串，其余字符精确匹配（不含路径分隔语义）。
func matchGlob(s, pattern string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return s == pattern
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	pos := len(parts[0])
	for _, seg := range parts[1 : len(parts)-1] {
		i := strings.Index(s[pos:], seg)
		if i < 0 {
			return false
		}
		pos += i + len(seg)
	}
	tail := parts[len(parts)-1]
	return len(s)-pos >= len(tail) && strings.HasSuffix(s[pos:], tail)
}
