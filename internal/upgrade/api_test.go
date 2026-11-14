package upgrade

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// WithAutoCheck(false)：CheckAndNotify 直接跳过，不访问 feed、不回调。
func TestWithAutoCheckDisabled(t *testing.T) {
	f := &fakeFetcher{rel: testRelease("0.2.0", "")}
	notifier := &recordNotifier{}
	svc := New(fakeHost{version: "0.1.0", dataDir: t.TempDir()},
		WithFetcher(f),
		WithFeedURL("https://feed.invalid/latest.json"),
		WithAutoCheck(false),
		WithNotifier(notifier),
	)
	res, err := svc.CheckAndNotify(context.Background())
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("err = %v, want ErrDisabled", err)
	}
	if res.Skipped != SkipDisabled {
		t.Fatalf("skipped = %q, want %q", res.Skipped, SkipDisabled)
	}
	if f.calls != 0 {
		t.Fatalf("fetcher called %d times, want 0", f.calls)
	}
	if len(notifier.updates) != 0 || notifier.noUpdate != 0 || len(notifier.errs) != 0 {
		t.Fatalf("notifier should stay silent: %+v", notifier)
	}
}

// 访问器：status 子命令与 wiring 层依赖，逐一覆盖。
func TestServiceAccessors(t *testing.T) {
	dataDir := t.TempDir()
	svc := New(fakeHost{version: "1.2.3", channel: "beta", dataDir: dataDir},
		WithFeedURL("https://feed.invalid/latest.json"),
		WithChannel("beta"),
		WithInterval(90*time.Minute),
	)
	if got := svc.CurrentVersion(); got != "1.2.3" {
		t.Fatalf("CurrentVersion = %q", got)
	}
	if got := svc.Channel(); got != "beta" {
		t.Fatalf("Channel = %q", got)
	}
	if got := svc.FeedURL(); got != "https://feed.invalid/latest.json" {
		t.Fatalf("FeedURL = %q", got)
	}
	if got := svc.Interval(); got != 90*time.Minute {
		t.Fatalf("Interval = %v", got)
	}
	if got := svc.DataDir(); got != dataDir {
		t.Fatalf("DataDir = %q, want %q", got, dataDir)
	}
}

// LastResult：Check 之后可读，且 With 克隆不影响原服务状态。
func TestLastResultAfterCheck(t *testing.T) {
	base := New(fakeHost{version: "0.1.0", dataDir: t.TempDir()},
		WithFetcher(&fakeFetcher{rel: testRelease("0.2.0", "0.1.5")}),
		WithFeedURL("https://feed.invalid/latest.json"),
	)
	if res, err := base.LastResult(); err != nil || res.Available {
		t.Fatalf("LastResult before any check = %+v, %v", res, err)
	}

	res, err := base.Check(context.Background())
	if err != nil || !res.Available || !res.Mandatory {
		t.Fatalf("Check = %+v, %v", res, err)
	}
	got, err := base.LastResult()
	if err != nil || !got.Available || got.Latest != "0.2.0" || !got.Mandatory {
		t.Fatalf("LastResult = %+v, %v", got, err)
	}

	// 已最新
	upToDate := New(fakeHost{version: "0.2.0", dataDir: t.TempDir()},
		WithFetcher(&fakeFetcher{rel: testRelease("0.2.0", "")}),
		WithFeedURL("https://feed.invalid/latest.json"),
	)
	if _, err := upToDate.Check(context.Background()); !errors.Is(err, ErrNoUpdate) {
		t.Fatalf("err = %v, want ErrNoUpdate", err)
	}
	if res, err := upToDate.LastResult(); !errors.Is(err, ErrNoUpdate) || res.Latest != "0.2.0" {
		t.Fatalf("LastResult = %+v, %v", res, err)
	}
}

// With 克隆保持 autoCheck（曾遗漏导致克隆后恒为 disabled）。
func TestWithPreservesAutoCheck(t *testing.T) {
	svc := New(fakeHost{version: "0.1.0", dataDir: t.TempDir()},
		WithFetcher(&fakeFetcher{rel: testRelease("0.2.0", "")}),
		WithFeedURL("https://feed.invalid/latest.json"),
		WithAutoCheck(true),
	)
	clone := svc.With(WithFeedURL("https://other.invalid/latest.json"))
	if _, err := clone.CheckThrottled(context.Background()); errors.Is(err, ErrDisabled) {
		t.Fatal("clone lost autoCheck=true")
	}
	if clone.FeedURL() != "https://other.invalid/latest.json" {
		t.Fatalf("clone FeedURL = %q", clone.FeedURL())
	}
	if svc.FeedURL() != "https://feed.invalid/latest.json" {
		t.Fatalf("original mutated: %q", svc.FeedURL())
	}
}

// NewApplier 返回可用实现；对不存在的安装包确定性报错（applier 前置 Stat 拦截，
// 避免 cmd /c start 触发系统 GUI 弹窗），且不得 panic。
func TestNewApplierAndInstallBadPath(t *testing.T) {
	a := NewApplier(io.Discard)
	if a == nil {
		t.Fatal("NewApplier returned nil")
	}
	missing := filepath.Join(t.TempDir(), "no-such-installer.exe")
	if err := a.Install(context.Background(), missing, testRelease("0.2.0", "")); err == nil {
		t.Fatal("Install missing installer = nil, want error")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("stat = %v, want IsNotExist", err)
	}
}
