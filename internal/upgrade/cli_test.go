package upgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

type fakeMaintainer struct {
	info      SchemaInfo
	backup    string
	backupErr error
}

func (m fakeMaintainer) BackupNow(context.Context) (string, error) { return m.backup, m.backupErr }
func (m fakeMaintainer) SchemaInfo(context.Context) (SchemaInfo, error) {
	return m.info, nil
}

func newCLIRunner(t *testing.T, host Host, rel Release, payload []byte) (*Runner, *fakeApplier, *fakeFetcher) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: insecureTLS()}}
	url, sum := artifactServer(t, payload, client)
	rel.Artifacts[0].URL = url
	rel.Artifacts[0].SHA256 = sum
	rel.Artifacts[0].Size = int64(len(payload))

	fetcher := &fakeFetcher{rel: rel}
	applier := &fakeApplier{}
	hook := &fakeHook{}
	svc := New(host,
		WithFetcher(fetcher),
		WithFeedURL("https://feed.invalid/latest.json"),
		WithChannel("stable"),
		WithHTTPClient(client),
		WithDownloadDir(t.TempDir()),
		WithApplier(applier),
		WithPreApply(hook),
	)
	r := &Runner{
		Svc:        svc,
		Maintainer: fakeMaintainer{info: SchemaInfo{Version: 1, Backups: []string{"sessions.v0-to-v1.20261018-093000.bak"}}},
		AutoCheck:  true,
		Out:        &bytes.Buffer{},
		ErrOut:     &bytes.Buffer{},
	}
	return r, applier, fetcher
}

func TestCLICheck(t *testing.T) {
	payload := []byte("installer")

	t.Run("update available exits 10", func(t *testing.T) {
		r, _, _ := newCLIRunner(t, fakeHost{version: "0.1.0", channel: "stable"}, testRelease("0.2.0", "0.1.5"), payload)
		if code := r.Run([]string{"check"}); code != ExitUpdateAvail {
			t.Fatalf("exit = %d, want %d\n%s", code, ExitUpdateAvail, r.Out)
		}
		out := r.Out.(*bytes.Buffer).String()
		for _, want := range []string{"当前版本: 0.1.0", "最新版本: 0.2.0", "强制升级", "upgrade apply"} {
			if !strings.Contains(out, want) {
				t.Fatalf("output missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("up to date exits 0", func(t *testing.T) {
		r, _, _ := newCLIRunner(t, fakeHost{version: "0.2.0", channel: "stable"}, testRelease("0.2.0", ""), payload)
		if code := r.Run([]string{"check"}); code != ExitOK {
			t.Fatalf("exit = %d, want 0\n%s", code, r.Out)
		}
		if !strings.Contains(r.Out.(*bytes.Buffer).String(), "已是最新版本") {
			t.Fatalf("output: %s", r.Out)
		}
	})

	t.Run("dev build exits 0", func(t *testing.T) {
		r, _, f := newCLIRunner(t, fakeHost{version: "dev", channel: "stable"}, testRelease("0.2.0", ""), payload)
		if code := r.Run([]string{"check"}); code != ExitOK {
			t.Fatalf("exit = %d, want 0", code)
		}
		if f.calls != 0 {
			t.Fatalf("dev build hit the feed %d times", f.calls)
		}
		if !strings.Contains(r.Out.(*bytes.Buffer).String(), "开发构建") {
			t.Fatalf("output: %s", r.Out)
		}
	})

	t.Run("error exits 1", func(t *testing.T) {
		r, _, _ := newCLIRunner(t, fakeHost{version: "0.1.0", channel: "stable"}, testRelease("0.2.0", ""), payload)
		r.Svc = r.Svc.With(WithFetcher(&fakeFetcher{err: errors.New("boom")}))
		if code := r.Run([]string{"check"}); code != ExitError {
			t.Fatalf("exit = %d, want 1", code)
		}
	})

	t.Run("json output", func(t *testing.T) {
		r, _, _ := newCLIRunner(t, fakeHost{version: "0.1.0", channel: "stable"}, testRelease("0.2.0", ""), payload)
		if code := r.Run([]string{"check", "--json"}); code != ExitUpdateAvail {
			t.Fatalf("exit = %d, want %d", code, ExitUpdateAvail)
		}
		var view checkView
		if err := json.Unmarshal(r.Out.(*bytes.Buffer).Bytes(), &view); err != nil {
			t.Fatalf("unmarshal: %v\n%s", err, r.Out)
		}
		if !view.Available || view.Current != "0.1.0" || view.Latest != "0.2.0" || view.Artifact == nil {
			t.Fatalf("view = %+v", view)
		}
	})

	t.Run("feed override", func(t *testing.T) {
		r, _, f := newCLIRunner(t, fakeHost{version: "0.1.0", channel: "stable"}, testRelease("0.2.0", ""), payload)
		if code := r.Run([]string{"check", "--feed", "https://other.invalid/latest.json"}); code != ExitUpdateAvail {
			t.Fatalf("exit = %d", code)
		}
		if f.gotURL != "https://other.invalid/latest.json" {
			t.Fatalf("fetched %q, want the override url", f.gotURL)
		}
	})
}

func TestCLIStatus(t *testing.T) {
	r, _, _ := newCLIRunner(t, fakeHost{version: "0.1.0", channel: "stable"}, testRelease("0.2.0", ""), nil)
	if code := r.Run([]string{"status"}); code != ExitOK {
		t.Fatalf("exit = %d, want 0\n%s", code, r.Out)
	}
	out := r.Out.(*bytes.Buffer).String()
	for _, want := range []string{"版本: 0.1.0", "通道: stable", "自动检查: 开", "数据 schema 版本: 1", "sessions.v0-to-v1."} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestCLIApply(t *testing.T) {
	payload := []byte("installer bytes")

	t.Run("with --yes installs", func(t *testing.T) {
		r, applier, _ := newCLIRunner(t, fakeHost{version: "0.1.0", channel: "stable"}, testRelease("0.2.0", ""), payload)
		if code := r.Run([]string{"apply", "--yes"}); code != ExitOK {
			t.Fatalf("exit = %d, want 0\n%s", code, r.Out)
		}
		if applier.calls != 1 {
			t.Fatalf("applier calls = %d, want 1", applier.calls)
		}
		if !strings.Contains(r.Out.(*bytes.Buffer).String(), "安装程序已启动") {
			t.Fatalf("output: %s", r.Out)
		}
	})

	t.Run("declined confirmation aborts", func(t *testing.T) {
		r, applier, _ := newCLIRunner(t, fakeHost{version: "0.1.0", channel: "stable"}, testRelease("0.2.0", ""), payload)
		r.Confirm = func(string) bool { return false }
		if code := r.Run([]string{"apply"}); code != ExitError {
			t.Fatalf("exit = %d, want 1", code)
		}
		if applier.calls != 0 {
			t.Fatalf("applier calls = %d, want 0", applier.calls)
		}
	})

	t.Run("already latest", func(t *testing.T) {
		r, applier, _ := newCLIRunner(t, fakeHost{version: "0.2.0", channel: "stable"}, testRelease("0.2.0", ""), payload)
		if code := r.Run([]string{"apply", "--yes"}); code != ExitOK {
			t.Fatalf("exit = %d, want 0", code)
		}
		if applier.calls != 0 {
			t.Fatalf("applier calls = %d, want 0", applier.calls)
		}
		if !strings.Contains(r.Out.(*bytes.Buffer).String(), "无需升级") {
			t.Fatalf("output: %s", r.Out)
		}
	})

	t.Run("json output", func(t *testing.T) {
		r, _, _ := newCLIRunner(t, fakeHost{version: "0.1.0", channel: "stable"}, testRelease("0.2.0", ""), payload)
		if code := r.Run([]string{"apply", "--yes", "--json"}); code != ExitOK {
			t.Fatalf("exit = %d, want 0", code)
		}
		var view applyView
		if err := json.Unmarshal(r.Out.(*bytes.Buffer).Bytes(), &view); err != nil {
			t.Fatalf("unmarshal: %v\n%s", err, r.Out)
		}
		if !view.Launched || view.Target != "0.2.0" {
			t.Fatalf("view = %+v", view)
		}
	})
}

func TestCLIDispatch(t *testing.T) {
	r, _, _ := newCLIRunner(t, fakeHost{version: "0.1.0", channel: "stable"}, testRelease("0.2.0", ""), nil)

	if code := r.Run(nil); code != ExitError {
		t.Fatalf("no args exit = %d, want 1", code)
	}
	if code := r.Run([]string{"bogus"}); code != ExitError {
		t.Fatalf("unknown subcommand exit = %d, want 1", code)
	}
	if code := r.Run([]string{"help"}); code != ExitOK {
		t.Fatalf("help exit = %d, want 0", code)
	}
}
