package document

// SSRF 校验测试：内网/环回/元数据地址必须被拒，公网地址放行。
// 域名解析经 lookupIP 接缝注入替身（测试不依赖真实网络）。

import (
	"net"
	"strings"
	"testing"
)

func TestValidatePublicURL(t *testing.T) {
	// 生产替身：example.com 解析为一个公网 IP
	origLookup := lookupIP
	lookupIP = func(host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	t.Cleanup(func() { lookupIP = origLookup })

	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"公网 https", "https://example.com/paper.pdf", false},
		{"公网 http 带端口", "http://example.com:8080/doc.pdf", false},
		{"localhost 拒绝", "http://localhost:7788/api", true},
		{"localhost 子域拒绝", "http://a.localhost/x", true},
		{"local 后缀拒绝", "http://nas.local/x", true},
		{"internal 后缀拒绝", "http://svc.internal/x", true},
		{"环回 IP 字面量", "http://127.0.0.1:7788/x", true},
		{"IPv6 环回", "http://[::1]/x", true},
		{"10/8 私网", "http://10.0.0.1/x", true},
		{"172.16/12 私网", "http://172.16.0.9/x", true},
		{"192.168/16 私网", "http://192.168.1.5/x", true},
		{"链路本地含云元数据地址", "http://169.254.169.254/latest/meta-data", true},
		{"非 http 协议", "file:///C:/Windows/system.ini", true},
		{"ftp 协议", "ftp://example.com/x", true},
		{"非 URL 输入", "随便一段话", true},
		{"空主机名", "http:///path", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePublicURL(tc.url)
			if tc.wantErr && err == nil {
				t.Errorf("ValidatePublicURL(%q) = nil, want error", tc.url)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("ValidatePublicURL(%q) = %v, want nil", tc.url, err)
			}
			// 拒绝内网时错误信息不得泄露"公网放行"的歧义（文案含"拒绝"或"仅支持"）
			if tc.wantErr && err != nil && !strings.Contains(err.Error(), "拒绝") &&
				!strings.Contains(err.Error(), "仅支持") && !strings.Contains(err.Error(), "无效") &&
				!strings.Contains(err.Error(), "解析失败") {
				t.Errorf("error message unclear: %v", err)
			}
		})
	}
}

func TestValidatePublicURLDNSRejections(t *testing.T) {
	cases := []struct {
		name string
		ips  []net.IP
	}{
		{"解析结果为私网", []net.IP{net.ParseIP("10.1.2.3")}},
		{"多 IP 之一为内网（全 IP 判定）", []net.IP{net.ParseIP("93.184.216.34"), net.ParseIP("192.168.0.1")}},
		{"解析结果为环回", []net.IP{net.ParseIP("127.0.0.1")}},
		{"IPv4-mapped IPv6 私网", []net.IP{net.ParseIP("::ffff:10.0.0.1")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			origLookup := lookupIP
			lookupIP = func(host string) ([]net.IP, error) { return tc.ips, nil }
			t.Cleanup(func() { lookupIP = origLookup })
			if err := ValidatePublicURL("https://rebind.example.com/x"); err == nil {
				t.Errorf("ValidatePublicURL with ips %v = nil, want error", tc.ips)
			}
		})
	}

	t.Run("域名解析失败拒绝", func(t *testing.T) {
		origLookup := lookupIP
		lookupIP = func(host string) ([]net.IP, error) { return nil, net.UnknownNetworkError("no such host") }
		t.Cleanup(func() { lookupIP = origLookup })
		if err := ValidatePublicURL("https://nonexistent.example.com/x"); err == nil {
			t.Fatal("DNS failure should be rejected (fail closed)")
		}
	})
}
