package upgrade

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newCLIRunnerRel 与 newCLIRunner 相同，但把「已指向 TLS 服务器」的 Release 交还调用方，
// 便于在保持下载地址可用的前提下改写 sha256 / url 构造失败场景。
func newCLIRunnerRel(t *testing.T, host Host, rel Release, payload []byte) (*Runner, *fakeApplier, Release) {
	t.Helper()
	r, applier, fetcher := newCLIRunner(t, host, rel, payload)
	return r, applier, fetcher.rel
}

// ---------- apply 失败路径（failApply 全覆盖） ----------

func TestCLIApplyDevBuild(t *testing.T) {
	payload := []byte("installer")

	t.Run("text", func(t *testing.T) {
		r, applier, _ := newCLIRunner(t, fakeHost{version: "dev", channel: "stable"}, testRelease("0.2.0", ""), payload)
		if code := r.Run([]string{"apply", "--yes"}); code != ExitError {
			t.Fatalf("exit = %d, want 1\n%s", code, r.Out)
		}
		if applier.calls != 0 {
			t.Fatalf("applier calls = %d, want 0", applier.calls)
		}
		if !strings.Contains(r.ErrOut.(*bytes.Buffer).String(), "开发构建") {
			t.Fatalf("stderr: %s", r.ErrOut)
		}
	})

	t.Run("json", func(t *testing.T) {
		r, applier, _ := newCLIRunner(t, fakeHost{version: "dev", channel: "stable"}, testRelease("0.2.0", ""), payload)
		if code := r.Run([]string{"apply", "--yes", "--json"}); code != ExitError {
			t.Fatalf("exit = %d, want 1\n%s", code, r.Out)
		}
		if applier.calls != 0 {
			t.Fatalf("applier calls = %d, want 0", applier.calls)
		}
		var view applyView
		if err := json.Unmarshal(r.Out.(*bytes.Buffer).Bytes(), &view); err != nil {
			t.Fatalf("unmarshal: %v\n%s", err, r.Out)
		}
		if view.Error != "dev build cannot be upgraded" || view.Launched {
			t.Fatalf("view = %+v", view)
		}
	})
}

// feed 中没有匹配当前平台的产物 → 下载前即失败。
func TestCLIApplyNoArtifact(t *testing.T) {
	payload := []byte("installer")
	r, applier, rel := newCLIRunnerRel(t, fakeHost{version: "0.1.0", channel: "stable"}, testRelease("0.2.0", ""), payload)

	rel.Artifacts = []Artifact{{Name: "absorb-agent.tar.gz", URL: "https://example.invalid/a.tgz", SHA256: testSHA}}
	r.Svc = r.Svc.With(WithFetcher(&fakeFetcher{rel: rel}))

	if code := r.Run([]string{"apply", "--yes"}); code != ExitError {
		t.Fatalf("exit = %d, want 1\n%s", code, r.Out)
	}
	if applier.calls != 0 {
		t.Fatalf("applier calls = %d, want 0", applier.calls)
	}
	if !strings.Contains(r.ErrOut.(*bytes.Buffer).String(), "no installable artifact") {
		t.Fatalf("stderr: %s", r.ErrOut)
	}
}

// 下载阶段失败（404）→ 不进校验、不安装。
func TestCLIApplyDownloadFailure(t *testing.T) {
	payload := []byte("installer")
	r, applier, rel := newCLIRunnerRel(t, fakeHost{version: "0.1.0", channel: "stable"}, testRelease("0.2.0", ""), payload)

	rel.Artifacts[0].URL = strings.Replace(rel.Artifacts[0].URL, "/setup.exe", "/missing.exe", 1)
	r.Svc = r.Svc.With(WithFetcher(&fakeFetcher{rel: rel}))

	if code := r.Run([]string{"apply", "--yes", "--json"}); code != ExitError {
		t.Fatalf("exit = %d, want 1\n%s", code, r.Out)
	}
	if applier.calls != 0 {
		t.Fatalf("applier calls = %d, want 0", applier.calls)
	}
	var view applyView
	if err := json.Unmarshal(r.Out.(*bytes.Buffer).Bytes(), &view); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(view.Error, "unexpected status 404") {
		t.Fatalf("view.Error = %q", view.Error)
	}
}

// 校验失败 → 删除临时文件、不安装。
func TestCLIApplyChecksumFailure(t *testing.T) {
	payload := []byte("installer")
	r, applier, rel := newCLIRunnerRel(t, fakeHost{version: "0.1.0", channel: "stable"}, testRelease("0.2.0", ""), payload)

	rel.Artifacts[0].SHA256 = strings.Repeat("f", 64) // 与真实内容不符
	r.Svc = r.Svc.With(WithFetcher(&fakeFetcher{rel: rel}))
	downloadDir := r.Svc.dl.Dir

	if code := r.Run([]string{"apply", "--yes"}); code != ExitError {
		t.Fatalf("exit = %d, want 1\n%s", code, r.Out)
	}
	if applier.calls != 0 {
		t.Fatalf("applier calls = %d, want 0", applier.calls)
	}
	if !strings.Contains(r.ErrOut.(*bytes.Buffer).String(), "checksum mismatch") {
		t.Fatalf("stderr: %s", r.ErrOut)
	}
	// 临时文件必须被清掉，不留未校验产物
	entries, err := os.ReadDir(downloadDir)
	if err != nil {
		t.Fatalf("read download dir: %v", err)
	}
	for _, e := range entries {
		t.Fatalf("leftover file in download dir: %s", e.Name())
	}
}

// ---------- 交互确认 ----------

func TestCLIConfirmStdin(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantRun bool
	}{
		{"y", "y\n", true},
		{"yes upper", "YES\n", true},
		{"n", "n\n", false},
		{"empty line", "\n", false},
		{"eof without newline", "y", true},
		{"eof empty", "", false},
		{"garbage", "whatever\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			payload := []byte("installer")
			r, applier, _ := newCLIRunner(t, fakeHost{version: "0.1.0", channel: "stable"}, testRelease("0.2.0", ""), payload)
			r.Stdin = strings.NewReader(c.in)

			code := r.Run([]string{"apply"})
			if c.wantRun {
				if code != ExitOK {
					t.Fatalf("exit = %d, want 0\n%s", code, r.Out)
				}
				if applier.calls != 1 {
					t.Fatalf("applier calls = %d, want 1", applier.calls)
				}
				if !strings.Contains(r.Out.(*bytes.Buffer).String(), "确认下载并安装") {
					t.Fatalf("prompt missing: %s", r.Out)
				}
				return
			}
			if code != ExitError {
				t.Fatalf("exit = %d, want 1", code)
			}
			if applier.calls != 0 {
				t.Fatalf("applier calls = %d, want 0", applier.calls)
			}
			if !strings.Contains(r.Out.(*bytes.Buffer).String(), "已取消") {
				t.Fatalf("cancel message missing: %s", r.Out)
			}
		})
	}
}

func TestCLIConfirmInjected(t *testing.T) {
	payload := []byte("installer")
	r, applier, _ := newCLIRunner(t, fakeHost{version: "0.1.0", channel: "stable"}, testRelease("0.2.0", ""), payload)

	var prompt string
	r.Confirm = func(p string) bool { prompt = p; return true }
	if code := r.Run([]string{"apply"}); code != ExitOK {
		t.Fatalf("exit = %d, want 0\n%s", code, r.Out)
	}
	if applier.calls != 1 {
		t.Fatalf("applier calls = %d, want 1", applier.calls)
	}
	if !strings.Contains(prompt, "0.2.0") {
		t.Fatalf("prompt = %q, want it to mention the target version", prompt)
	}
}

// ---------- status 分支 ----------

func TestCLIStatusJSON(t *testing.T) {
	r, _, _ := newCLIRunner(t, fakeHost{version: "0.1.0", channel: "stable"}, testRelease("0.2.0", ""), nil)
	if code := r.Run([]string{"status", "--json"}); code != ExitOK {
		t.Fatalf("exit = %d, want 0\n%s", code, r.Out)
	}
	var view statusView
	if err := json.Unmarshal(r.Out.(*bytes.Buffer).Bytes(), &view); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, r.Out)
	}
	if view.Version != "0.1.0" || view.Channel != "stable" || !view.AutoCheck {
		t.Fatalf("view = %+v", view)
	}
	if view.IntervalHours != 6 || view.SchemaVersion != 1 || len(view.Backups) != 1 {
		t.Fatalf("view = %+v", view)
	}
}

func TestCLIStatusAutoCheckOff(t *testing.T) {
	r, _, _ := newCLIRunner(t, fakeHost{version: "0.1.0", channel: "stable"}, testRelease("0.2.0", ""), nil)
	r.AutoCheck = false
	if code := r.Run([]string{"status"}); code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(r.Out.(*bytes.Buffer).String(), "自动检查: 关") {
		t.Fatalf("output: %s", r.Out)
	}
}

func TestCLIStatusLastCheck(t *testing.T) {
	dataDir := t.TempDir()
	stamp := filepath.Join(dataDir, "upgrade", "last_check")
	if err := os.MkdirAll(filepath.Dir(stamp), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	when := time.Now().Add(-time.Hour).Round(time.Second)
	if err := os.WriteFile(stamp, []byte(strconv.FormatInt(when.Unix(), 10)), 0o600); err != nil {
		t.Fatalf("write stamp: %v", err)
	}

	r, _, _ := newCLIRunner(t, fakeHost{version: "0.1.0", channel: "stable", dataDir: dataDir}, testRelease("0.2.0", ""), nil)
	if code := r.Run([]string{"status"}); code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	out := r.Out.(*bytes.Buffer).String()
	if !strings.Contains(out, "上次检查") || !strings.Contains(out, when.Format(time.RFC3339)) {
		t.Fatalf("output missing last check:\n%s", out)
	}
}

func TestCLIStatusNoMaintainer(t *testing.T) {
	r, _, _ := newCLIRunner(t, fakeHost{version: "0.1.0", channel: "stable"}, testRelease("0.2.0", ""), nil)
	r.Maintainer = nil
	if code := r.Run([]string{"status"}); code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	out := r.Out.(*bytes.Buffer).String()
	if strings.Contains(out, "数据 schema 版本") {
		t.Fatalf("should not print schema info without maintainer:\n%s", out)
	}
	if !strings.Contains(out, "安装包暂存") {
		t.Fatalf("output: %s", out)
	}
}

func TestCLIStatusNoBackups(t *testing.T) {
	r, _, _ := newCLIRunner(t, fakeHost{version: "0.1.0", channel: "stable"}, testRelease("0.2.0", ""), nil)
	r.Maintainer = fakeMaintainer{info: SchemaInfo{Version: 1}}
	if code := r.Run([]string{"status"}); code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(r.Out.(*bytes.Buffer).String(), "迁移备份: 无") {
		t.Fatalf("output: %s", r.Out)
	}
}

// ---------- check 分支 ----------

func TestCLICheckPublishedAt(t *testing.T) {
	rel := testRelease("0.2.0", "")
	rel.PublishedAt = time.Date(2026, 10, 18, 9, 30, 0, 0, time.UTC)
	// 2048 字节载荷 → Size 2048 → humanBytes 走 KiB 分支
	payload := bytes.Repeat([]byte("x"), 2048)
	r, _, _ := newCLIRunner(t, fakeHost{version: "0.1.0", channel: "stable"}, rel, payload)
	if code := r.Run([]string{"check"}); code != ExitUpdateAvail {
		t.Fatalf("exit = %d, want %d", code, ExitUpdateAvail)
	}
	out := r.Out.(*bytes.Buffer).String()
	for _, want := range []string{
		"发布时间: 2026-10-18T09:30:00Z",
		"安装包: absorb-agent-setup-0.2.0.exe (2.0 KiB)",
		"发布说明: https://example.invalid/notes",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestCLICheckDevBuildJSON(t *testing.T) {
	r, _, _ := newCLIRunner(t, fakeHost{version: "dev", channel: "stable"}, testRelease("0.2.0", ""), nil)
	if code := r.Run([]string{"check", "--json"}); code != ExitOK {
		t.Fatalf("exit = %d, want 0\n%s", code, r.Out)
	}
	var view checkView
	if err := json.Unmarshal(r.Out.(*bytes.Buffer).Bytes(), &view); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if view.Error != "dev build" || view.Available {
		t.Fatalf("view = %+v", view)
	}
}

// ---------- 输出口与格式化 ----------

// nil Out/ErrOut 不得 panic（回落系统标准输出）。
func TestCLIOutputFallback(t *testing.T) {
	r := &Runner{Svc: New(fakeHost{version: "0.1.0"}, WithFeedURL("https://feed.invalid/latest.json"))}
	if code := r.Run([]string{"help"}); code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if w := r.out(); w != os.Stdout {
		t.Fatalf("out() = %v, want os.Stdout", w)
	}
	if w := r.errOut(); w != os.Stderr {
		t.Fatalf("errOut() = %v, want os.Stderr", w)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{2048, "2.0 KiB"},
		{1536, "1.5 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{1024 * 1024 * 1024, "1.0 GiB"},
		{1024 * 1024 * 1024 * 1024, "1.0 TiB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.in); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}
