package fetch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/tool"
	"trpc.group/trpc-go/trpc-agent-go/tool/webfetch/httpfetch"
)

// testTimeout 单测默认超时（经 timeoutFn 动态注入，与生产路径同构）
const testTimeout = 30 * time.Second

func testTimeoutFn() func() time.Duration { return func() time.Duration { return testTimeout } }

// pageFixture 返回一个带标题/正文/列表/代码块的测试页面
func pageFixture() string {
	return `<!DOCTYPE html>
<html>
<head><title>测试页面</title></head>
<body>
<nav>导航噪音链接1 导航噪音链接2</nav>
<article>
<h1>这是标题</h1>
<p>这是正文内容，需要被提取出来。</p>
<ul>
<li>第一项</li>
<li>第二项</li>
</ul>
<pre><code>fmt.Println("hello")</code></pre>
</article>
</body>
</html>`
}

// withBody 启动一个返回给定 HTML 的测试服务
func withBody(t *testing.T, contentType, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Write([]byte(body))
	}))
}

// call 通过 httpfetch 工具调用单个 URL，返回原始输出结构
func call(t *testing.T, inner tool.CallableTool, url string) (content, errMsg string) {
	t.Helper()
	args, err := json.Marshal(map[string][]string{"urls": {url}})
	if err != nil {
		t.Fatalf("构造参数失败: %v", err)
	}
	raw, err := inner.Call(context.Background(), args)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	bts, _ := json.Marshal(raw)
	var wrap struct {
		Results []struct {
			Content string `json:"content"`
			Error   string `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal(bts, &wrap); err != nil {
		t.Fatalf("解析结果失败: %v", err)
	}
	if len(wrap.Results) == 0 {
		return "", "无结果"
	}
	return wrap.Results[0].Content, wrap.Results[0].Error
}

func TestFetchPage_ToolName(t *testing.T) {
	ft, err := NewFetchPageTool(func() string { return "" }, testTimeoutFn(), nil)
	if err != nil {
		t.Fatalf("构建工具失败: %v", err)
	}
	decl := ft.Declaration()
	if decl.Name != "fetch_page" {
		t.Fatalf("工具名应为 fetch_page, got: %s", decl.Name)
	}
	if decl.Description == "" {
		t.Fatal("工具应有描述")
	}
}

// TestProxyResolver_Invalid 代理地址非法时请求期降级为直连（返回 nil URL）
func TestProxyResolver_Invalid(t *testing.T) {
	pf := ProxyResolver(func() string { return "://bad" })
	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	u, err := pf(req)
	if err != nil || u != nil {
		t.Fatalf("非法代理应降级直连, got url=%v err=%v", u, err)
	}
}

// TestProxyResolver_Valid 有效代理按 getter 当前值生效，值变更即时跟随
func TestProxyResolver_Valid(t *testing.T) {
	addr := "http://127.0.0.1:7897"
	pf := ProxyResolver(func() string { return addr })
	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	u, err := pf(req)
	if err != nil || u == nil || u.String() != addr {
		t.Fatalf("应返回当前代理地址, got url=%v err=%v", u, err)
	}
	addr = "http://127.0.0.1:7890"
	u, err = pf(req)
	if err != nil || u == nil || u.String() != addr {
		t.Fatalf("getter 变更后应跟随新地址, got url=%v err=%v", u, err)
	}
}

// ===== 系统代理：ProxyServer 解析 / 例外地址匹配 / 优先级 =====

func TestParseProxyServer(t *testing.T) {
	cases := []struct{ in, want string }{
		{"127.0.0.1:7897", "http://127.0.0.1:7897"},
		{"", ""},
		{"http://p:8080", "http://p:8080"},
		{"http=1.1.1.1:80;https=2.2.2.2:443;ftp=3.3.3.3:21", "http://2.2.2.2:443"},
		{"http=1.1.1.1:80", "http://1.1.1.1:80"},
		{"socks=127.0.0.1:1080", "socks5://127.0.0.1:1080"},
		{"ftp=1.1.1.1:21", ""},
	}
	for _, c := range cases {
		if got := parseProxyServer(c.in); got != c.want {
			t.Errorf("parseProxyServer(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHostBypassed(t *testing.T) {
	cases := []struct {
		host, override string
		want           bool
	}{
		{"localhost", "localhost;127.*", true},
		{"127.0.0.1", "localhost;127.*", true},
		{"192.168.1.5", "localhost;192.168.*;<local>", true},
		{"mypc", "<local>", true},
		{"mypc.local", "<local>", false},
		{"a.example.com", "*.example.com", true},
		{"b.example.com", "example.com", false},
		{"localhost:8080", "localhost", true},
		{"example.com", "", false},
		{"example.com", "localhost;127.*", false},
	}
	for _, c := range cases {
		if got := hostBypassed(c.host, c.override); got != c.want {
			t.Errorf("hostBypassed(%q, %q) = %v, want %v", c.host, c.override, got, c.want)
		}
	}
}

// TestProxyResolver_SystemProxy 优先级：手动配置 > 系统代理（含例外直连）> 环境变量回退
func TestProxyResolver_SystemProxy(t *testing.T) {
	orig := systemProxyLookup
	t.Cleanup(func() { systemProxyLookup = orig })
	sysURL, _ := url.Parse("http://sys-proxy:7897")
	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)

	// 手动配置优先，不触发系统代理探测
	called := false
	systemProxyLookup = func(*url.URL) (*url.URL, bool) { called = true; return sysURL, true }
	pf := ProxyResolver(func() string { return "http://manual:1" })
	u, err := pf(req)
	if err != nil || u == nil || u.Host != "manual:1" || called {
		t.Fatalf("手动配置应优先: url=%v err=%v called=%v", u, err, called)
	}

	// 手动为空 → 系统代理生效
	systemProxyLookup = func(*url.URL) (*url.URL, bool) { return sysURL, true }
	pf = ProxyResolver(func() string { return "" })
	u, err = pf(req)
	if err != nil || u == nil || u.Host != "sys-proxy:7897" {
		t.Fatalf("应使用系统代理: url=%v err=%v", u, err)
	}

	// 命中例外地址 → 直连
	systemProxyLookup = func(*url.URL) (*url.URL, bool) { return nil, true }
	u, err = pf(req)
	if err != nil || u != nil {
		t.Fatalf("例外地址应直连: url=%v err=%v", u, err)
	}

	// 未检测到系统代理 → 回退环境变量（不做 URL 断言：net/http 对环境变量进程内一次性缓存）
	systemProxyLookup = func(*url.URL) (*url.URL, bool) { return nil, false }
	if _, err = pf(req); err != nil {
		t.Fatalf("回退环境变量不应报错: %v", err)
	}
}

func TestFetchPage_HTMLContent(t *testing.T) {
	srv := withBody(t, "text/html; charset=utf-8", pageFixture())
	defer srv.Close()
	ft, err := NewFetchPageTool(func() string { return "" }, testTimeoutFn(), nil)
	if err != nil {
		t.Fatalf("构建工具失败: %v", err)
	}
	args, _ := json.Marshal(map[string]string{"url": srv.URL})
	raw, err := ft.Call(context.Background(), args)
	if err != nil {
		t.Fatalf("Call 失败: %v", err)
	}
	bts, _ := json.Marshal(raw)
	out := string(bts)
	for _, k := range []string{"这是标题", "这是正文内容", "第一项", "hello"} {
		if !strings.Contains(out, k) {
			t.Errorf("内容缺少关键信息 %q\n完整输出: %s", k, out)
		}
	}
}

func TestFetchPage_NonHTMLContent(t *testing.T) {
	srv := withBody(t, "text/plain; charset=utf-8", "Hello, 这是一段纯文本。")
	defer srv.Close()
	out := fetchViaURL(t, srv.URL)
	if !out.Success {
		t.Fatalf("应成功, got: %s", out.Message)
	}
	if !strings.Contains(out.Content, "Hello") {
		t.Fatalf("纯文本应返回, got: %s", out.Content)
	}
}

func TestFetchPage_JSONContent(t *testing.T) {
	srv := withBody(t, "application/json", `{"name":"test","value":123}`)
	defer srv.Close()
	out := fetchViaURL(t, srv.URL)
	if !out.Success {
		t.Fatalf("应成功, got: %s", out.Message)
	}
	if !strings.Contains(out.Content, "test") || !strings.Contains(out.Content, "123") {
		t.Fatalf("JSON 应返回, got: %s", out.Content)
	}
}

func TestFetchPage_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("error"))
	}))
	defer srv.Close()
	out := fetchViaURL(t, srv.URL)
	if out.Success {
		t.Fatal("服务端错误应返回 Success=false")
	}
	if out.Message == "" {
		t.Fatal("应返回失败原因")
	}
}

func TestFetchPage_EmptyURL(t *testing.T) {
	out := fetchViaURL(t, "")
	if out.Success {
		t.Fatal("空 URL 应返回 Success=false")
	}
}

func TestFetchPage_InvalidProtocol(t *testing.T) {
	for _, u := range []string{"chrome://settings", "file:///etc/passwd", "ftp://example.com"} {
		out := fetchViaURL(t, u)
		if out.Success {
			t.Fatalf("非法协议 %s 应被拒绝", u)
		}
	}
}

// fetchViaURL 通过 NewFetchPageTool 的 JSON 调用路径获取结果
func fetchViaURL(t *testing.T, url string) fetchPageOutput {
	t.Helper()
	ft, err := NewFetchPageTool(func() string { return "" }, testTimeoutFn(), nil)
	if err != nil {
		t.Fatalf("构建工具失败: %v", err)
	}
	args, _ := json.Marshal(map[string]string{"url": url})
	raw, err := ft.Call(context.Background(), args)
	if err != nil {
		t.Fatalf("Call 失败: %v", err)
	}
	bts, _ := json.Marshal(raw)
	var out fetchPageOutput
	if err := json.Unmarshal(bts, &out); err != nil {
		t.Fatalf("解析输出失败: %v", err)
	}
	return out
}

// ===== 输出截断：防超长正文挤占上下文窗口 =====

func TestTruncateRunes(t *testing.T) {
	// 未超限：原样返回
	short := strings.Repeat("好", 100)
	if got := truncateRunes(short, fetchMaxRunes); got != short {
		t.Errorf("未超长应原样返回")
	}
	// 超限：截断到 n 个字符并追加标注（按 rune 计，不切坏多字节字符）
	long := strings.Repeat("好", fetchMaxRunes+500)
	got := truncateRunes(long, fetchMaxRunes)
	if got := len([]rune(got)); got > fetchMaxRunes+50 {
		t.Errorf("截断后长度应接近 %d，got %d", fetchMaxRunes, got)
	}
	if !strings.Contains(got, "已截断") {
		t.Errorf("截断结果应包含标注。got 尾部: %q", got[len(got)-40:])
	}
}

func TestFetchPage_TruncatesLongContent(t *testing.T) {
	// 超长纯文本 → 输出被截断并带标注
	long := strings.Repeat("这是一段超长正文内容。", 5000) // 55000 字 > 20000
	srv := withBody(t, "text/plain; charset=utf-8", long)
	defer srv.Close()
	out := fetchViaURL(t, srv.URL)
	if !out.Success {
		t.Fatalf("应成功, got: %s", out.Message)
	}
	if runes := len([]rune(out.Content)); runes > fetchMaxRunes+50 {
		t.Errorf("输出应被截断到 %d 字符左右, got %d", fetchMaxRunes, runes)
	}
	if !strings.Contains(out.Content, "已截断") {
		t.Errorf("截断输出应包含标注")
	}
	// 正常长度内容不受影响
	srv2 := withBody(t, "text/plain; charset=utf-8", "短内容")
	defer srv2.Close()
	out2 := fetchViaURL(t, srv2.URL)
	if !out2.Success || strings.Contains(out2.Content, "已截断") {
		t.Errorf("正常长度内容不应被截断, got: %s", out2.Content)
	}
}

// 以下测试直接验证底层 httpfetch 的「主内容提取开/关」差异，作为真实数据的补充
func TestMainContent_OnRemovesNav(t *testing.T) {
	srv := withBody(t, "text/html; charset=utf-8", pageFixture())
	defer srv.Close()

	on := httpfetch.NewTool(httpfetch.WithMainContentExtraction(true))
	onContent, _ := call(t, on, srv.URL)

	off := httpfetch.NewTool(httpfetch.WithMainContentExtraction(false))
	offContent, _ := call(t, off, srv.URL)

	// 开主内容应剔除 <nav> 噪音
	if strings.Contains(onContent, "导航噪音链接") {
		t.Logf("[提示] 主内容提取未剔除导航：%s", aLe(onContent))
		t.Errorf("开主内容提取后应剔除导航噪音")
	}
	// 关主内容应保留导航
	if !strings.Contains(offContent, "导航噪音链接") {
		t.Errorf("关主内容提取后应保留导航噪音")
	}
	t.Logf("开主内容长度=%d, 关主内容长度=%d", len(onContent), len(offContent))
	t.Logf("开主内容输出: %s", onContent)
	t.Logf("关主内容输出: %s", offContent)
}

func aLe(s string) string {
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}
