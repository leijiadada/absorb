package document

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// SSRF 校验：/api/page/prepare 运行在本机桌面环境，URL 经浏览器扩展转发
// （源头是任意网页可诱导的输入）。必须拒绝环回/内网/链路本地/云元数据目标，
// 防止本地服务被网页当作 SSRF 跳板访问 localhost 服务或 169.254.169.254。
//
// 已知限制（如实声明）：FetchPageService 底层 httpfetch 自持 HTTP 客户端并
// 跟随重定向，无法注入每一跳的校验；本层校验入口 URL（含 DNS 解析结果的全部
// IP）。重定向跳向内网的防护依赖目标站点可信，后续如需严格闭环可改为自持
// 客户端逐跳校验后再交给提取器。

// lookupIP 域名解析接缝（测试注入替身；生产即 net.LookupIP）。
var lookupIP = net.LookupIP

// ValidatePublicURL 校验 URL 仅指向公网 http/https 资源：
//   - 仅接受 http/https（拒绝 file/ftp/data 等）；
//   - 主机名黑名单：localhost（含子域）、.local、.internal；
//   - IP 字面量与 DNS 解析结果逐 IP 判定：拒绝环回/私网/链路本地/组播/未指定
//     （覆盖 127.0.0.1、10/8、172.16/12、192.168/16、169.254/16 含云元数据地址）。
//
// DNS rebinding TOCTOU（校验后 httpfetch 重新解析到内网 IP）暂不处理，同上属
// 逐跳校验的后续增强范围。
func ValidatePublicURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return errors.New("URL 格式无效")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("仅支持 http/https 链接（当前 %s:）", u.Scheme)
	}
	host := u.Hostname() // 去端口；IPv6 去方括号
	if host == "" {
		return errors.New("URL 缺少主机名")
	}
	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") ||
		strings.HasSuffix(lower, ".local") || strings.HasSuffix(lower, ".internal") {
		return fmt.Errorf("拒绝访问本机/内网地址: %s", host)
	}
	// IP 字面量直接判定（不依赖解析）；域名解析后逐 IP 判定
	if ip := net.ParseIP(host); ip != nil {
		if !isPublicIP(ip) {
			return fmt.Errorf("拒绝访问非公网地址: %s", host)
		}
		return nil
	}
	ips, err := lookupIP(host)
	if err != nil {
		return fmt.Errorf("域名解析失败: %v", err)
	}
	for _, ip := range ips {
		if !isPublicIP(ip) {
			return fmt.Errorf("拒绝访问非公网地址: %s → %s", host, ip)
		}
	}
	return nil
}

// isPublicIP 判定是否公网 IP（环回/私网/链路本地/组播/未指定均视为非公网）。
func isPublicIP(ip net.IP) bool {
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified())
}
