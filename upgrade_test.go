package main

import (
	"net/http"
	"net/url"
	"testing"

	"absorb-agent/internal/conf"
)

// req 构造 Proxy 决策所需的最小请求（http.Transport.Proxy 以 URL scheme/host 为输入）。
func req(scheme, host string) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, scheme+"://"+host+"/x", nil)
	return r
}

// proxyURL 调 transport.Proxy 并断言返回可解析的代理地址。
func proxyURL(t *testing.T, proxy func(*http.Request) (*url.URL, error), r *http.Request) *url.URL {
	t.Helper()
	u, err := proxy(r)
	if err != nil {
		t.Fatalf("proxy lookup: %v", err)
	}
	return u
}

// 手填裸 host:port 代理（WinINET 同款形态）必须被规范化为可用的 http:// 代理，
// 而不是 url.Parse 失败后被静默丢弃变成直连（历史 bug：feed 不可直连即 net_fail）。
func TestUpgradeHTTPClient_BareProxyAddr(t *testing.T) {
	var cfg conf.Config
	cfg.Proxy.URL = "127.0.0.1:7897"
	client := newUpgradeHTTPClient(cfg)
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", client.Transport)
	}
	got := proxyURL(t, tr.Proxy, req("https", "raw.githubusercontent.com"))
	if got == nil {
		t.Fatal("proxy = nil, want http://127.0.0.1:7897")
	}
	if got.Host != "127.0.0.1:7897" {
		t.Fatalf("proxy host = %q, want 127.0.0.1:7897", got.Host)
	}
}

// 无法解析的代理值必须显式回退环境变量（与空代理一致），且行为与 ProxyFromEnvironment 相同。
func TestUpgradeHTTPClient_UnparseableProxyFallback(t *testing.T) {
	var cfg conf.Config
	cfg.Proxy.URL = "http://bad host with spaces"
	client := newUpgradeHTTPClient(cfg)
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", client.Transport)
	}
	r := req("https", "example.com")
	got, err := tr.Proxy(r)
	if err != nil {
		t.Fatalf("proxy lookup: %v", err)
	}
	want, _ := http.ProxyFromEnvironment(r)
	if (got == nil) != (want == nil) || (got != nil && want != nil && got.String() != want.String()) {
		t.Fatalf("fallback proxy = %v, want env behavior %v", got, want)
	}
}
