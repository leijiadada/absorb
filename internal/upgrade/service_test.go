package upgrade

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// ---------- 测试替身（全部经接口注入，升级模块不感知其存在） ----------

type fakeHost struct {
	version string
	channel string
	dataDir string
}

func (h fakeHost) Version() string { return h.version }
func (h fakeHost) Channel() string { return h.channel }
func (h fakeHost) DataDir() string { return h.dataDir }

// timedHost 额外提供构建时间（BuildInfo）。
type timedHost struct {
	fakeHost
	builtAt string
}

func (h timedHost) BuildTime() string { return h.builtAt }

type fakeFetcher struct {
	mu     sync.Mutex
	rel    Release
	err    error
	calls  int
	gotURL string
}

func (f *fakeFetcher) Fetch(_ context.Context, url string) (Release, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.gotURL = url
	return f.rel, f.err
}

type fakeApplier struct {
	mu    sync.Mutex
	calls int
	path  string
	rel   Release
	err   error
}

func (a *fakeApplier) Install(_ context.Context, pkg string, rel Release) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	a.path = pkg
	a.rel = rel
	return a.err
}

type fakeHook struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (h *fakeHook) BeforeApply(context.Context, Release) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	return h.err
}

type recordNotifier struct {
	mu       sync.Mutex
	updates  []Release
	progress []int
	noUpdate int
	errs     []error
}

func (n *recordNotifier) OnUpdateAvailable(rel Release) {
	n.mu.Lock()
	n.updates = append(n.updates, rel)
	n.mu.Unlock()
}
func (n *recordNotifier) OnDownloading(pct int) {
	n.mu.Lock()
	n.progress = append(n.progress, pct)
	n.mu.Unlock()
}
func (n *recordNotifier) OnNoUpdate(_, _ string) {
	n.mu.Lock()
	n.noUpdate++
	n.mu.Unlock()
}
func (n *recordNotifier) OnError(err error) {
	n.mu.Lock()
	n.errs = append(n.errs, err)
	n.mu.Unlock()
}

// ---------- Check ----------

func TestServiceCheck(t *testing.T) {
	const feed = "https://feed.invalid/latest.json"

	newSvc := func(t *testing.T, host Host, f FeedFetcher, opts ...Option) *Service {
		t.Helper()
		base := []Option{
			WithFetcher(f), WithFeedURL(feed), WithChannel("stable"),
			WithDownloadDir(t.TempDir()),
		}
		return New(host, append(base, opts...)...)
	}

	t.Run("update available", func(t *testing.T) {
		f := &fakeFetcher{rel: testRelease("0.2.0", "")}
		res, err := newSvc(t, fakeHost{version: "0.1.0", channel: "stable"}, f).Check(context.Background())
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if !res.Available || res.Mandatory || res.Latest != "0.2.0" {
			t.Fatalf("res = %+v", res)
		}
		if f.gotURL != feed {
			t.Fatalf("fetched %q, want %q", f.gotURL, feed)
		}
	})

	t.Run("mandatory when below minimum", func(t *testing.T) {
		f := &fakeFetcher{rel: testRelease("0.2.0", "0.1.5")}
		res, err := newSvc(t, fakeHost{version: "0.1.0", channel: "stable"}, f).Check(context.Background())
		if err != nil || !res.Available || !res.Mandatory {
			t.Fatalf("res = %+v, err = %v", res, err)
		}
	})

	t.Run("not mandatory when at or above minimum", func(t *testing.T) {
		f := &fakeFetcher{rel: testRelease("0.2.0", "0.1.0")}
		res, err := newSvc(t, fakeHost{version: "0.1.0", channel: "stable"}, f).Check(context.Background())
		if err != nil || res.Mandatory {
			t.Fatalf("res = %+v, err = %v", res, err)
		}
	})

	t.Run("up to date", func(t *testing.T) {
		f := &fakeFetcher{rel: testRelease("0.2.0", "")}
		_, err := newSvc(t, fakeHost{version: "0.2.0", channel: "stable"}, f).Check(context.Background())
		if !errors.Is(err, ErrNoUpdate) {
			t.Fatalf("err = %v, want ErrNoUpdate", err)
		}
	})

	t.Run("feed older than current", func(t *testing.T) {
		f := &fakeFetcher{rel: testRelease("0.1.0", "")}
		_, err := newSvc(t, fakeHost{version: "0.2.0", channel: "stable"}, f).Check(context.Background())
		if !errors.Is(err, ErrNoUpdate) {
			t.Fatalf("err = %v, want ErrNoUpdate", err)
		}
	})

	t.Run("channel mismatch", func(t *testing.T) {
		f := &fakeFetcher{rel: testRelease("0.2.0", "")}
		svc := New(fakeHost{version: "0.1.0", channel: "beta"},
			WithFetcher(f), WithFeedURL(feed), WithChannel("beta"), WithDownloadDir(t.TempDir()))
		if _, err := svc.Check(context.Background()); !errors.Is(err, ErrBadFeed) {
			t.Fatalf("err = %v, want ErrBadFeed", err)
		}
	})

	t.Run("dev build", func(t *testing.T) {
		f := &fakeFetcher{rel: testRelease("0.2.0", "")}
		res, err := newSvc(t, fakeHost{version: "dev", channel: "stable"}, f).Check(context.Background())
		if !errors.Is(err, ErrDevBuild) || res.Skipped != SkipDevBuild {
			t.Fatalf("res = %+v, err = %v", res, err)
		}
		if f.calls != 0 {
			t.Fatalf("dev build should not hit the feed, got %d calls", f.calls)
		}
	})

	t.Run("no feed url", func(t *testing.T) {
		f := &fakeFetcher{rel: testRelease("0.2.0", "")}
		svc := New(fakeHost{version: "0.1.0"}, WithFetcher(f), WithDownloadDir(t.TempDir()))
		if _, err := svc.Check(context.Background()); !errors.Is(err, ErrNoFeedURL) {
			t.Fatalf("err = %v, want ErrNoFeedURL", err)
		}
	})

	t.Run("fetcher error propagates", func(t *testing.T) {
		f := &fakeFetcher{err: errors.New("network down")}
		if _, err := newSvc(t, fakeHost{version: "0.1.0", channel: "stable"}, f).Check(context.Background()); err == nil {
			t.Fatal("expected error")
		}
	})
}

// ---------- 节流 ----------

func TestServiceCheckThrottled(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	f := &fakeFetcher{rel: testRelease("0.2.0", "")}
	svc := New(fakeHost{version: "0.1.0", channel: "stable", dataDir: dataDir},
		WithFetcher(f), WithFeedURL("https://feed.invalid/latest.json"),
		WithInterval(time.Hour), WithClock(clock), WithDownloadDir(t.TempDir()))

	// 首次：无戳，允许并成功
	if _, err := svc.CheckThrottled(context.Background()); err != nil {
		t.Fatalf("first check: %v", err)
	}
	if _, ok := svc.LastCheck(); !ok {
		t.Fatal("stamp not written after successful check")
	}

	// 间隔内：跳过，不再访问 feed
	now = now.Add(30 * time.Minute)
	res, err := svc.CheckThrottled(context.Background())
	if !errors.Is(err, ErrThrottled) || res.Skipped != SkipThrottled {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if f.calls != 1 {
		t.Fatalf("feed calls = %d, want 1 (throttled check must not fetch)", f.calls)
	}

	// 间隔后：恢复
	now = now.Add(31 * time.Minute)
	if _, err := svc.CheckThrottled(context.Background()); err != nil {
		t.Fatalf("check after interval: %v", err)
	}
	if f.calls != 2 {
		t.Fatalf("feed calls = %d, want 2", f.calls)
	}
}

func TestThrottleNotWrittenOnNetworkError(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	f := &fakeFetcher{err: errors.New("boom")}
	svc := New(fakeHost{version: "0.1.0", channel: "stable", dataDir: dataDir},
		WithFetcher(f), WithFeedURL("https://feed.invalid/latest.json"),
		WithInterval(time.Hour), WithClock(func() time.Time { return now }), WithDownloadDir(t.TempDir()))

	for i := 0; i < 2; i++ {
		now = now.Add(10 * time.Minute)
		if _, err := svc.CheckThrottled(context.Background()); err == nil {
			t.Fatal("expected network error")
		}
	}
	if _, ok := svc.LastCheck(); ok {
		t.Fatal("stamp must not be written when the feed is unreachable")
	}
	if f.calls != 2 {
		t.Fatalf("feed calls = %d, want 2 (no throttle after failures)", f.calls)
	}
}

// ---------- Apply ----------

// artifactServer 起一个 https 服务器提供安装包字节，返回 URL 与对应 SHA256。
func artifactServer(t *testing.T, payload []byte, client *http.Client) (string, string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/setup.exe", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", itoa(len(payload)))
		_, _ = w.Write(payload)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL + "/setup.exe", sha256Hex(payload)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func newApplyService(t *testing.T, host Host, rel Release, client *http.Client, hook PreApplyHook) (*Service, *fakeApplier) {
	t.Helper()
	applier := &fakeApplier{}
	svc := New(host,
		WithFetcher(&fakeFetcher{rel: rel}),
		WithFeedURL("https://feed.invalid/latest.json"),
		WithChannel("stable"),
		WithHTTPClient(client),
		WithDownloadDir(t.TempDir()),
		WithApplier(applier),
		WithPreApply(hook),
	)
	return svc, applier
}

func TestServiceApplySuccess(t *testing.T) {
	payload := []byte("fake installer payload")
	srv, client := tlsServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	})
	_ = srv
	url, sum := artifactServer(t, payload, client)

	rel := testRelease("0.2.0", "")
	rel.Artifacts[0].URL = url
	rel.Artifacts[0].SHA256 = sum
	rel.Artifacts[0].Size = int64(len(payload))

	notifier := &recordNotifier{}
	hook := &fakeHook{}
	svc, applier := newApplyService(t, fakeHost{version: "0.1.0", channel: "stable"}, rel, client, hook)
	svc.notifier = notifier

	if err := svc.Apply(context.Background(), rel); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if applier.calls != 1 {
		t.Fatalf("applier calls = %d, want 1", applier.calls)
	}
	if hook.calls != 1 {
		t.Fatalf("pre-apply calls = %d, want 1", hook.calls)
	}
	if filepath.Base(applier.path) != rel.Artifacts[0].Name {
		t.Fatalf("installed %q, want %q", applier.path, rel.Artifacts[0].Name)
	}
	if len(notifier.progress) < 2 || notifier.progress[0] != 0 || notifier.progress[len(notifier.progress)-1] != 100 {
		t.Fatalf("progress = %v, want first 0 and last 100", notifier.progress)
	}
}

func TestServiceApplyChecksumMismatch(t *testing.T) {
	payload := []byte("fake installer payload")
	srv, client := tlsServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	})
	_ = srv
	url, _ := artifactServer(t, payload, client)

	rel := testRelease("0.2.0", "")
	rel.Artifacts[0].URL = url
	rel.Artifacts[0].SHA256 = testSHA // 故意错

	hook := &fakeHook{}
	svc, applier := newApplyService(t, fakeHost{version: "0.1.0", channel: "stable"}, rel, client, hook)

	err := svc.Apply(context.Background(), rel)
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("err = %v, want ErrChecksum", err)
	}
	if applier.calls != 0 {
		t.Fatalf("applier must not run on checksum mismatch, got %d calls", applier.calls)
	}
	if hook.calls != 0 {
		t.Fatalf("pre-apply must not run on checksum mismatch, got %d calls", hook.calls)
	}
	// 未校验产物必须被清掉
	entries, rerr := filepath.Glob(filepath.Join(svc.dl.Dir, "*"))
	if rerr != nil {
		t.Fatalf("glob: %v", rerr)
	}
	if len(entries) != 0 {
		t.Fatalf("leftover files after checksum failure: %v", entries)
	}
}

func TestServiceApplyHookFailureAborts(t *testing.T) {
	payload := []byte("fake installer payload")
	srv, client := tlsServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	})
	_ = srv
	url, sum := artifactServer(t, payload, client)

	rel := testRelease("0.2.0", "")
	rel.Artifacts[0].URL = url
	rel.Artifacts[0].SHA256 = sum

	hook := &fakeHook{err: errors.New("backup failed")}
	svc, applier := newApplyService(t, fakeHost{version: "0.1.0", channel: "stable"}, rel, client, hook)

	if err := svc.Apply(context.Background(), rel); err == nil {
		t.Fatal("expected error from pre-apply hook")
	}
	if applier.calls != 0 {
		t.Fatalf("applier must not run when pre-apply fails, got %d calls", applier.calls)
	}
}

func TestServiceApplyInsecureArtifactRejected(t *testing.T) {
	rel := testRelease("0.2.0", "")
	rel.Artifacts[0].URL = "http://example.invalid/setup.exe"
	svc, applier := newApplyService(t, fakeHost{version: "0.1.0", channel: "stable"}, rel, nil, &fakeHook{})

	if err := svc.Apply(context.Background(), rel); !errors.Is(err, ErrInsecureURL) {
		t.Fatalf("err = %v, want ErrInsecureURL", err)
	}
	if applier.calls != 0 {
		t.Fatalf("applier calls = %d, want 0", applier.calls)
	}
}

// ---------- CheckAndNotify ----------

func TestServiceCheckAndNotify(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

	newSvc := func(t *testing.T, f FeedFetcher, n *recordNotifier) *Service {
		return New(fakeHost{version: "0.1.0", channel: "stable", dataDir: dataDir},
			WithFetcher(f), WithFeedURL("https://feed.invalid/latest.json"),
			WithInterval(time.Hour), WithClock(func() time.Time { return now }),
			WithDownloadDir(t.TempDir()), WithNotifier(n))
	}

	t.Run("available notifies", func(t *testing.T) {
		n := &recordNotifier{}
		if _, err := newSvc(t, &fakeFetcher{rel: testRelease("0.2.0", "")}, n).CheckAndNotify(context.Background()); err != nil {
			t.Fatalf("CheckAndNotify: %v", err)
		}
		if len(n.updates) != 1 {
			t.Fatalf("updates = %d, want 1", len(n.updates))
		}
	})

	t.Run("no update notifies once", func(t *testing.T) {
		n := &recordNotifier{}
		now = now.Add(2 * time.Hour) // 越过上一子测试写入的节流戳
		if _, err := newSvc(t, &fakeFetcher{rel: testRelease("0.1.0", "")}, n).CheckAndNotify(context.Background()); !errors.Is(err, ErrNoUpdate) {
			t.Fatalf("err = %v, want ErrNoUpdate", err)
		}
		if n.noUpdate != 1 || len(n.updates) != 0 {
			t.Fatalf("noUpdate = %d, updates = %d", n.noUpdate, len(n.updates))
		}
	})

	t.Run("throttled stays silent", func(t *testing.T) {
		n := &recordNotifier{}
		now = now.Add(30 * time.Minute) // 间隔内：节流
		if _, err := newSvc(t, &fakeFetcher{rel: testRelease("0.2.0", "")}, n).CheckAndNotify(context.Background()); !errors.Is(err, ErrThrottled) {
			t.Fatalf("err = %v, want ErrThrottled", err)
		}
		if len(n.updates) != 0 || n.noUpdate != 0 || len(n.errs) != 0 {
			t.Fatalf("throttled check must be silent: %+v", n)
		}
	})

	t.Run("error notifies", func(t *testing.T) {
		n := &recordNotifier{}
		now = now.Add(2 * time.Hour)
		if _, err := newSvc(t, &fakeFetcher{err: errors.New("boom")}, n).CheckAndNotify(context.Background()); err == nil {
			t.Fatal("expected error")
		}
		if len(n.errs) != 1 {
			t.Fatalf("errors = %d, want 1", len(n.errs))
		}
	})
}
