package upgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------- CLI 剩余分支 ----------

// apply 的 JSON 变体：无匹配产物 / 已最新。
func TestCLIApplyJSONVariants(t *testing.T) {
	payload := []byte("installer")

	t.Run("no artifact", func(t *testing.T) {
		r, _, rel := newCLIRunnerRel(t, fakeHost{version: "0.1.0", channel: "stable"}, testRelease("0.2.0", ""), payload)
		rel.Artifacts = []Artifact{{Name: "absorb.tar.gz", URL: "https://example.invalid/a.tgz", SHA256: testSHA}}
		r.Svc = r.Svc.With(WithFetcher(&fakeFetcher{rel: rel}))
		if code := r.Run([]string{"apply", "--yes", "--json"}); code != ExitError {
			t.Fatalf("exit = %d, want 1\n%s", code, r.Out)
		}
		var view applyView
		if err := json.Unmarshal(r.Out.(*bytes.Buffer).Bytes(), &view); err != nil {
			t.Fatalf("unmarshal: %v\n%s", err, r.Out)
		}
		if view.Error == "" || view.Launched {
			t.Fatalf("view = %+v", view)
		}
	})

	t.Run("already latest", func(t *testing.T) {
		r, _, _ := newCLIRunner(t, fakeHost{version: "0.2.0", channel: "stable"}, testRelease("0.2.0", ""), payload)
		if code := r.Run([]string{"apply", "--yes", "--json"}); code != ExitOK {
			t.Fatalf("exit = %d, want 0\n%s", code, r.Out)
		}
		var view applyView
		if err := json.Unmarshal(r.Out.(*bytes.Buffer).Bytes(), &view); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if view.Error != "" || view.Launched || view.Current != "0.2.0" {
			t.Fatalf("view = %+v", view)
		}
	})
}

// --channel 覆盖：与 feed 一致时放行，不一致时报错。
func TestCLIServiceChannelOverride(t *testing.T) {
	payload := []byte("installer")

	t.Run("matching", func(t *testing.T) {
		r, _, _ := newCLIRunner(t, fakeHost{version: "0.1.0", channel: "beta"}, testRelease("0.2.0", ""), payload)
		rel := testRelease("0.2.0", "")
		rel.Channel = "beta"
		r.Svc = r.Svc.With(WithFetcher(&fakeFetcher{rel: rel}))
		if code := r.Run([]string{"check", "--channel", "beta"}); code != ExitUpdateAvail {
			t.Fatalf("exit = %d, want %d\n%s", code, ExitUpdateAvail, r.Out)
		}
	})

	t.Run("mismatch", func(t *testing.T) {
		r, _, _ := newCLIRunner(t, fakeHost{version: "0.1.0", channel: "stable"}, testRelease("0.2.0", ""), payload)
		if code := r.Run([]string{"check", "--channel", "beta"}); code != ExitError {
			t.Fatalf("exit = %d, want 1", code)
		}
		if !strings.Contains(r.ErrOut.(*bytes.Buffer).String(), "channel") {
			t.Fatalf("stderr: %s", r.ErrOut)
		}
	})
}

// ---------- download 剩余分支 ----------

func TestDownloaderDefaultClient(t *testing.T) {
	// nil Client → 走默认客户端（连不上即报错，但覆盖 client() 分支）
	d := &Downloader{Dir: t.TempDir()}
	art := Artifact{Name: "setup.exe", URL: "https://127.0.0.1:1/setup.exe", SHA256: testSHA}
	if _, err := d.Fetch(context.Background(), art); err == nil {
		t.Fatal("expected connection error")
	}
}

func TestDownloaderBadRequestURL(t *testing.T) {
	d := &Downloader{Dir: t.TempDir()}
	art := Artifact{Name: "setup.exe", URL: "https://exa\x7fmple.invalid/setup.exe", SHA256: testSHA}
	if _, err := d.Fetch(context.Background(), art); err == nil {
		t.Fatal("expected error on invalid url")
	}
}

// 目标文件名已被目录占用 → Rename 失败且不留 .part。
func TestDownloaderRenameFails(t *testing.T) {
	payload := []byte("installer")
	srv, client := tlsServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	})
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "setup.exe"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	d := &Downloader{Client: client, Dir: dir}
	art := Artifact{Name: "setup.exe", URL: srv.URL + "/setup.exe", SHA256: sha256Hex(payload)}
	if _, err := d.Fetch(context.Background(), art); err == nil {
		t.Fatal("expected rename error")
	}
	if _, err := os.Stat(filepath.Join(dir, "setup.exe.part")); !os.IsNotExist(err) {
		t.Fatalf("stat = %v, want IsNotExist (.part should be cleaned)", err)
	}
}

// 临时文件名已被目录占用 → 创建失败。
func TestDownloaderCreatePartFails(t *testing.T) {
	payload := []byte("installer")
	srv, client := tlsServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	})
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "setup.exe.part"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	d := &Downloader{Client: client, Dir: dir}
	art := Artifact{Name: "setup.exe", URL: srv.URL + "/setup.exe", SHA256: sha256Hex(payload)}
	if _, err := d.Fetch(context.Background(), art); err == nil {
		t.Fatal("expected create temp file error")
	}
}

// 长度未知（无 Content-Length）时 total <= 0，进度仍上报。
func TestDownloaderProgressUnknownLength(t *testing.T) {
	srv, client := tlsServer(t, func(w http.ResponseWriter, _ *http.Request) {
		// 不设置 Content-Length，分块写出
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("abc"))
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = w.Write([]byte("def"))
	})
	var seen int
	var lastTotal int64 = 1
	d := &Downloader{Client: client, Dir: t.TempDir(), Progress: func(done, total int64) {
		seen++
		lastTotal = total
	}}
	payload := []byte("abcdef")
	art := Artifact{Name: "setup.exe", URL: srv.URL + "/setup.exe", SHA256: sha256Hex(payload)}
	if _, err := d.Fetch(context.Background(), art); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if seen == 0 {
		t.Fatal("progress never reported")
	}
	if lastTotal > 0 {
		t.Fatalf("total = %d, want <= 0 for unknown length", lastTotal)
	}
}

// ---------- checker 剩余分支 ----------

func TestCheckerDefaultClient(t *testing.T) {
	f := &HTTPFetcher{} // nil Client → 默认客户端
	if _, err := f.Fetch(context.Background(), "https://127.0.0.1:1/latest.json"); err == nil {
		t.Fatal("expected connection error")
	}
}

func TestCheckerBadRequestURL(t *testing.T) {
	f := NewHTTPFetcher(5 * time.Second)
	if _, err := f.Fetch(context.Background(), "https://exa\x7fmple.invalid/latest.json"); err == nil {
		t.Fatal("expected error on invalid url")
	}
}

// ---------- service 剩余分支 ----------

func TestWithHTTPClientNil(t *testing.T) {
	svc := New(fakeHost{version: "0.1.0"}, WithFeedURL("https://feed.invalid/latest.json"), WithHTTPClient(nil))
	if svc.dl.Client != nil {
		t.Fatalf("dl.Client = %v, want nil", svc.dl.Client)
	}
	// nil 客户端下仍能走默认客户端（连不上即报错）
	if _, err := svc.dl.Fetch(context.Background(),
		Artifact{Name: "a.exe", URL: "https://127.0.0.1:1/a.exe", SHA256: testSHA}); err == nil {
		t.Fatal("expected error")
	}
}

// 安装器本身失败 → Apply 原样返回错误（备份已做，安装包已就绪）。
func TestServiceApplyApplierError(t *testing.T) {
	payload := []byte("fake installer payload")
	srv, client := tlsServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	})
	applier := &fakeApplier{err: errors.New("boom")}
	svc, _ := newApplyService(t, fakeHost{version: "0.1.0"}, testRelease("0.2.0", ""), client, &fakeHook{})

	rel := testRelease("0.2.0", "")
	rel.Artifacts[0].URL = srv.URL + "/setup.exe"
	rel.Artifacts[0].SHA256 = sha256Hex(payload)
	rel.Artifacts[0].Size = int64(len(payload))
	svc.applier = applier

	err := svc.Apply(context.Background(), rel)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want containing boom", err)
	}
	if applier.calls != 1 {
		t.Fatalf("applier calls = %d, want 1", applier.calls)
	}
}

// 未注入 PreApplyHook 时 Apply 也能跑完（备份是可选能力）。
func TestServiceApplyWithoutHook(t *testing.T) {
	payload := []byte("fake installer payload")
	srv, client := tlsServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	})
	svc, applier := newApplyService(t, fakeHost{version: "0.1.0"}, testRelease("0.2.0", ""), client, nil)

	rel := testRelease("0.2.0", "")
	rel.Artifacts[0].URL = srv.URL + "/setup.exe"
	rel.Artifacts[0].SHA256 = sha256Hex(payload)
	rel.Artifacts[0].Size = int64(len(payload))

	if err := svc.Apply(context.Background(), rel); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if applier.calls != 1 {
		t.Fatalf("applier calls = %d, want 1", applier.calls)
	}
}

// ---------- throttle 剩余分支 ----------

// 戳目录被文件占用 → Touch 报错（不静默）。
func TestThrottleTouchMkdirFails(t *testing.T) {
	file := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	th := NewThrottle(file, time.Hour, nil)
	if err := th.Touch(); err == nil {
		t.Fatal("expected error when stamp dir cannot be created")
	}
}

// pct 进度百分比：长度未知（total<=0）返回 0，达到/超过总数返回 100。
func TestPct(t *testing.T) {
	cases := []struct {
		done, total int64
		want        int
	}{
		{0, 0, 0},
		{5, 0, 0},
		{5, -1, 0},
		{0, 100, 0},
		{50, 100, 50},
		{99, 100, 99},
		{100, 100, 100},
		{150, 100, 100}, // 超额钳制
	}
	for _, c := range cases {
		if got := pct(c.done, c.total); got != c.want {
			t.Errorf("pct(%d,%d) = %d, want %d", c.done, c.total, got, c.want)
		}
	}
}

// 数据层不可用：status 必须显式报错并返回非 0（真实环境曾因静默降级而"看起来正常"）。
func TestCLIDataLayerBroken(t *testing.T) {
	payload := []byte("installer")
	broken := errors.New("open storage: database schema v99 is newer than this build v1")

	t.Run("status reports and fails", func(t *testing.T) {
		r, _, _ := newCLIRunner(t, fakeHost{version: "0.1.0", channel: "stable"}, testRelease("0.2.0", ""), payload)
		r.Maintainer = nil
		r.DataErr = broken
		if code := r.Run([]string{"status"}); code != ExitError {
			t.Fatalf("exit = %d, want 1\n%s", code, r.Out)
		}
		out := r.ErrOut.(*bytes.Buffer).String()
		if !strings.Contains(out, "数据层不可用") || !strings.Contains(out, "newer than this build") {
			t.Fatalf("stderr: %s", out)
		}
	})

	t.Run("apply refuses without backup", func(t *testing.T) {
		r, applier, _ := newCLIRunner(t, fakeHost{version: "0.1.0", channel: "stable"}, testRelease("0.2.0", ""), payload)
		r.Maintainer = nil
		r.DataErr = broken
		if code := r.Run([]string{"apply", "--yes"}); code != ExitError {
			t.Fatalf("exit = %d, want 1\n%s", code, r.Out)
		}
		if applier.calls != 0 {
			t.Fatalf("applier calls = %d, want 0 (must not install without backup)", applier.calls)
		}
		if !strings.Contains(r.ErrOut.(*bytes.Buffer).String(), "拒绝在无法备份的情况下升级") {
			t.Fatalf("stderr: %s", r.ErrOut)
		}
	})

	t.Run("check still works without data layer", func(t *testing.T) {
		r, _, _ := newCLIRunner(t, fakeHost{version: "0.1.0", channel: "stable"}, testRelease("0.2.0", ""), payload)
		r.Maintainer = nil
		r.DataErr = broken
		if code := r.Run([]string{"check"}); code != ExitUpdateAvail {
			t.Fatalf("exit = %d, want %d", code, ExitUpdateAvail)
		}
	})
}

// ---------- semver 剩余分支 ----------

// 字母标识 vs 数字标识（compareIdent 的对称分支）。
func TestCompareIdentAlphaVsNumeric(t *testing.T) {
	a, err := ParseVersion("1.0.0-alpha")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	b, err := ParseVersion("1.0.0-1")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := a.Compare(b); got != 1 {
		t.Fatalf("Compare(alpha, numeric) = %d, want 1", got)
	}
	if got := b.Compare(a); got != -1 {
		t.Fatalf("Compare(numeric, alpha) = %d, want -1", got)
	}
}
