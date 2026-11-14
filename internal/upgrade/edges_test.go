package upgrade

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------- download ----------

func TestDownloaderErrors(t *testing.T) {
	payload := []byte("installer bytes")
	srv, client := tlsServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	})
	sum := sha256Hex(payload)

	cases := []struct {
		name    string
		mutate  func(*Downloader, *Artifact)
		wantErr string
	}{
		{"http scheme", func(_ *Downloader, a *Artifact) { a.URL = "http://example.invalid/a.exe" }, "only https"},
		{"ftp scheme", func(_ *Downloader, a *Artifact) { a.URL = "ftp://example.invalid/a.exe" }, "only https"},
		{"empty dir", func(d *Downloader, _ *Artifact) { d.Dir = "" }, "download dir not configured"},
		{"dot name", func(_ *Downloader, a *Artifact) { a.Name = "." }, "bad artifact name"},
		{"dotdot name", func(_ *Downloader, a *Artifact) { a.Name = ".." }, "bad artifact name"},
		{"checksum", func(_ *Downloader, a *Artifact) { a.SHA256 = strings.Repeat("0", 64) }, "checksum mismatch"},
		{"sha uppercase ok", func(_ *Downloader, a *Artifact) { a.SHA256 = strings.ToUpper(sum) }, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			d := &Downloader{Client: client, Dir: dir}
			art := Artifact{Name: "setup.exe", URL: srv.URL + "/setup.exe", SHA256: sum, Size: int64(len(payload))}
			c.mutate(d, &art)

			path, err := d.Fetch(context.Background(), art)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if path == "" {
					t.Fatal("empty path")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, c.wantErr)
			}
			// 失败不得留下 .part 或成品
			entries, rerr := os.ReadDir(dir)
			if rerr != nil {
				t.Fatalf("read dir: %v", rerr)
			}
			for _, e := range entries {
				t.Fatalf("leftover file after failure: %s", e.Name())
			}
		})
	}
}

// 404：需要路径感知的服务器（tlsServer 对所有路径都返回 200）。
func TestDownloaderNotFound(t *testing.T) {
	payload := []byte("installer bytes")
	mux := http.NewServeMux()
	mux.HandleFunc("/setup.exe", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	})
	srv, client := tlsServer(t, mux.ServeHTTP)
	d := &Downloader{Client: client, Dir: t.TempDir()}
	art := Artifact{Name: "setup.exe", URL: srv.URL + "/missing.exe", SHA256: sha256Hex(payload)}

	if _, err := d.Fetch(context.Background(), art); err == nil {
		t.Fatal("expected error on 404")
	} else if !strings.Contains(err.Error(), "unexpected status 404") {
		t.Fatalf("err = %v, want containing \"unexpected status 404\"", err)
	}
	entries, err := os.ReadDir(d.Dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		t.Fatalf("leftover file: %s", e.Name())
	}
}

// 路径穿越的产物名被 Base 收敛到暂存目录内，不会写到目录外。
func TestDownloaderNameTraversalSanitized(t *testing.T) {
	payload := []byte("installer bytes")
	srv, client := tlsServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	})
	dir := t.TempDir()
	d := &Downloader{Client: client, Dir: dir}
	art := Artifact{Name: "../evil.exe", URL: srv.URL + "/setup.exe", SHA256: sha256Hex(payload)}

	path, err := d.Fetch(context.Background(), art)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if filepath.Dir(path) != dir {
		t.Fatalf("path = %q, want it inside %q", path, dir)
	}
	if filepath.Base(path) != "evil.exe" {
		t.Fatalf("base = %q, want evil.exe", filepath.Base(path))
	}
	// 目录外不得出现文件
	entries, err := os.ReadDir(filepath.Dir(dir))
	if err != nil {
		t.Fatalf("read parent: %v", err)
	}
	for _, e := range entries {
		if e.Name() == "evil.exe" || strings.HasSuffix(e.Name(), ".part") {
			t.Fatalf("escaped the download dir: %s", e.Name())
		}
	}
}

// 下载中断（服务端声明长度大于实际写出）→ 报错且无残留。
func TestDownloaderCopyInterrupted(t *testing.T) {
	srv, client := tlsServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "4096")
		_, _ = w.Write([]byte("short"))
	})
	d := &Downloader{Client: client, Dir: t.TempDir()}
	art := Artifact{Name: "setup.exe", URL: srv.URL + "/setup.exe", SHA256: strings.Repeat("a", 64)}

	if _, err := d.Fetch(context.Background(), art); err == nil {
		t.Fatal("expected error on truncated download")
	}
	entries, err := os.ReadDir(d.Dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		t.Fatalf("leftover file: %s", e.Name())
	}
}

// 暂存目录不可建（路径是已存在的文件）→ 明确报错。
func TestDownloaderMkdirFails(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	d := &Downloader{Dir: file}
	art := Artifact{Name: "setup.exe", URL: "https://example.invalid/a.exe", SHA256: testSHA}
	if _, err := d.Fetch(context.Background(), art); err == nil {
		t.Fatal("expected error when download dir cannot be created")
	}
}

// 进度回调：末次进度等于文件长度；长度已知时 total 与长度一致。
func TestDownloaderProgress(t *testing.T) {
	payload := []byte("0123456789")
	srv, client := tlsServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	})
	var lastDone, lastTotal int64 = -1, -1
	d := &Downloader{Client: client, Dir: t.TempDir(), Progress: func(done, total int64) {
		lastDone, lastTotal = done, total
	}}
	art := Artifact{Name: "setup.exe", URL: srv.URL + "/setup.exe", SHA256: sha256Hex(payload)}
	if _, err := d.Fetch(context.Background(), art); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if lastDone != int64(len(payload)) {
		t.Fatalf("last done = %d, want %d", lastDone, len(payload))
	}
	if lastTotal > 0 && lastTotal != int64(len(payload)) {
		t.Fatalf("last total = %d, want %d", lastTotal, len(payload))
	}
}

// ---------- checker ----------

func TestCheckerBadJSON(t *testing.T) {
	srv, client := tlsServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("this is not json"))
	})
	f := NewHTTPFetcher(5 * time.Second)
	f.Client = client
	if _, err := f.Fetch(context.Background(), srv.URL); err == nil {
		t.Fatal("expected error on malformed feed body")
	}
}

func TestCheckerEmptyVersion(t *testing.T) {
	srv, client := tlsServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"version":"","channel":"stable","artifacts":[{"name":"a.exe","url":"https://x.invalid/a.exe","sha256":"` + testSHA + `"}]}`))
	})
	f := NewHTTPFetcher(5 * time.Second)
	f.Client = client
	if _, err := f.Fetch(context.Background(), srv.URL); err == nil {
		t.Fatal("expected error on empty version")
	}
}

func TestCheckerStatusVariants(t *testing.T) {
	for _, code := range []int{http.StatusForbidden, http.StatusInternalServerError} {
		srv, client := tlsServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		})
		f := NewHTTPFetcher(5 * time.Second)
		f.Client = client
		if _, err := f.Fetch(context.Background(), srv.URL); err == nil {
			t.Fatalf("status %d: expected error", code)
		}
	}
}

// ---------- semver ----------

func TestSemverEdges(t *testing.T) {
	parseCases := []struct {
		in      string
		wantErr bool
		dev     bool
	}{
		{"0.1.0", false, false},
		{"v0.1.0", false, false},
		{"V0.1.0", false, false},
		{"  0.1.0  ", false, false},
		{"1.2.3.4", false, false}, // 四段（扩展版本）
		{"0.1.0+build.5", false, false},
		{"0.1.0-beta.1", false, false},
		{"0.1.0-rc-2", false, false},
		{"dev", false, true},
		{"0.0.0-dev-abc123", false, true},
		{"0.1.0-dirty", false, true},
		{"", true, false},
		{"0.1", true, false},
		{"0.1.0.0.0", true, false},
		{"a.b.c", true, false},
		{"0.1.x", true, false},
		{"1.2.3.x", true, false}, // 第四段非数字
		{"0.1.0-", true, false},  // 空的预-release 标识
		{"0.1.0-beta..1", true, false},
		{"0.1.0-beta_1", true, false}, // 下划线不合法
	}
	for _, c := range parseCases {
		v, err := ParseVersion(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseVersion(%q) = %+v, want error", c.in, v)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseVersion(%q): %v", c.in, err)
			continue
		}
		if v.Dev != c.dev {
			t.Errorf("ParseVersion(%q).Dev = %v, want %v", c.in, v.Dev, c.dev)
		}
	}

	compareCases := []struct {
		a, b string
		want int
	}{
		{"0.9.0", "0.10.0", -1}, // 数值比较而非字典序
		{"1.0.0", "1.0.0", 0},
		{"1.0.0", "1.0.0+build", 0}, // 构建元数据不参与比较
		{"1.0.0", "1.0.0-beta", 1},  // 正式版 > 预发布
		{"1.0.0-beta.1", "1.0.0-beta.2", -1},
		{"1.0.0-beta.2", "1.0.0-beta.10", -1}, // 数字标识数值比较
		{"1.0.0-alpha", "1.0.0-beta", -1},     // 字母标识字典序
		{"1.0.0-1", "1.0.0-alpha", -1},        // 数字标识 < 字母标识
		{"1.0.0-beta", "1.0.0-beta.1", -1},    // 前缀相等时更短更小
	}
	for _, c := range compareCases {
		av, err := ParseVersion(c.a)
		if err != nil {
			t.Fatalf("parse %q: %v", c.a, err)
		}
		bv, err := ParseVersion(c.b)
		if err != nil {
			t.Fatalf("parse %q: %v", c.b, err)
		}
		if got := av.Compare(bv); got != c.want {
			t.Errorf("Compare(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}

	extCases := []struct{ in, want string }{
		{"0.2.0", "0.2.0.0"},
		{"0.2.0-beta.1", "0.2.0.1"},
		{"0.2.0-beta", "0.2.0.1"},
		{"dev", "0.0.0.0"},
		{"garbage", "0.0.0.0"},
	}
	for _, c := range extCases {
		if got := ExtensionVersion(c.in); got != c.want {
			t.Errorf("ExtensionVersion(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---------- throttle ----------

func TestThrottleEdges(t *testing.T) {
	t.Run("dir empty means no throttle", func(t *testing.T) {
		th := NewThrottle("", time.Hour, nil)
		if _, ok := th.Last(); ok {
			t.Fatal("Last should be false with empty dir")
		}
		if !th.Allow() {
			t.Fatal("Allow should be true with empty dir")
		}
		if err := th.Touch(); err != nil {
			t.Fatalf("Touch: %v", err)
		}
	})

	t.Run("touch overwrites", func(t *testing.T) {
		now := time.Unix(1000, 0)
		th := NewThrottle(t.TempDir(), time.Hour, func() time.Time { return now })
		if err := th.Touch(); err != nil {
			t.Fatalf("Touch: %v", err)
		}
		now = now.Add(time.Minute)
		if err := th.Touch(); err != nil {
			t.Fatalf("Touch again: %v", err)
		}
		got, ok := th.Last()
		if !ok || !got.Equal(now) {
			t.Fatalf("Last = %v (%v), want %v", got, ok, now)
		}
		entries, _ := os.ReadDir(filepath.Join(th.dir, "upgrade"))
		if len(entries) != 1 {
			t.Fatalf("stamp files = %d, want 1", len(entries))
		}
	})

	t.Run("corrupt stamp", func(t *testing.T) {
		dir := t.TempDir()
		stamp := filepath.Join(dir, "upgrade", "last_check")
		if err := os.MkdirAll(filepath.Dir(stamp), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(stamp, []byte("not-a-number"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		th := NewThrottle(dir, time.Hour, nil)
		if _, ok := th.Last(); ok {
			t.Fatal("Last should be false for corrupt stamp")
		}
		if !th.Allow() {
			t.Fatal("Allow should be true for corrupt stamp (fail open)")
		}
	})

	t.Run("missing dir", func(t *testing.T) {
		th := NewThrottle(filepath.Join(t.TempDir(), "nope"), time.Hour, nil)
		if _, ok := th.Last(); ok {
			t.Fatal("Last should be false when dir missing")
		}
		if !th.Allow() {
			t.Fatal("Allow should be true when dir missing")
		}
	})

	t.Run("interval", func(t *testing.T) {
		now := time.Unix(1000, 0)
		dir := t.TempDir()
		th := NewThrottle(dir, time.Hour, func() time.Time { return now })
		if err := th.Touch(); err != nil {
			t.Fatalf("Touch: %v", err)
		}
		if th.Allow() {
			t.Fatal("should be throttled right after touch")
		}
		now = now.Add(30 * time.Minute)
		if th.Allow() {
			t.Fatal("should still be throttled at 30m")
		}
		now = now.Add(31 * time.Minute)
		if !th.Allow() {
			t.Fatal("should be allowed past the interval")
		}
	})
}

// ---------- feed 边角 ----------

func TestPickArtifactEdges(t *testing.T) {
	cases := []struct {
		name      string
		artifacts []Artifact
		want      string // 期望选中的名字；空 = 期望报错
	}{
		{
			name: "goos+goarch wins over goos only",
			artifacts: []Artifact{
				{Name: "absorb-windows.exe"},
				{Name: "absorb-windows-amd64.exe"},
			},
			want: "absorb-windows-amd64.exe",
		},
		{
			name:      "goos only when no arch match",
			artifacts: []Artifact{{Name: "absorb-windows.zip"}},
			want:      "absorb-windows.zip",
		},
		{
			name: "fallback first exe",
			artifacts: []Artifact{
				{Name: "absorb-setup.exe"},
				{Name: "absorb-other.exe"},
			},
			want: "absorb-setup.exe",
		},
		{
			name:      "no exe and no platform token",
			artifacts: []Artifact{{Name: "absorb.tar.gz"}},
			want:      "",
		},
		{"empty", nil, ""},
		{"name case insensitive", []Artifact{{Name: "ABSORB-WINDOWS-AMD64.EXE"}}, "ABSORB-WINDOWS-AMD64.EXE"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rel := Release{Artifacts: c.artifacts}
			got, err := rel.PickArtifact("windows", "amd64")
			if c.want == "" {
				if err == nil {
					t.Fatalf("expected error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Name != c.want {
				t.Fatalf("picked %q, want %q", got.Name, c.want)
			}
		})
	}
}

func TestIsSHA256HexEdges(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{testSHA, true},
		{strings.ToUpper(testSHA), true},
		{"  " + testSHA + "  ", true},
		{strings.Repeat("a", 63), false},
		{strings.Repeat("a", 65), false},
		{strings.Repeat("g", 64), false},
		{strings.Repeat("a", 63) + "!", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isSHA256Hex(c.in); got != c.want {
			t.Errorf("isSHA256Hex(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestIsHTTPSEdges(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"https://a.invalid/x", true},
		{"HTTPS://A.INVALID/X", true},
		{"  https://a.invalid/x  ", true},
		{"http://a.invalid/x", false},
		{"ftp://a.invalid/x", false},
		{"", false},
		{"a.invalid/x", false},
	}
	for _, c := range cases {
		if got := isHTTPS(c.in); got != c.want {
			t.Errorf("isHTTPS(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
