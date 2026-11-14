package upgrade

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

const testSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func testRelease(version, minimum string) Release {
	return Release{
		Version:        version,
		Channel:        "stable",
		MinimumVersion: minimum,
		NotesURL:       "https://example.invalid/notes",
		Artifacts: []Artifact{{
			Name:   "absorb-agent-setup-" + version + ".exe",
			URL:    "https://example.invalid/setup.exe",
			SHA256: testSHA,
			Size:   2048,
		}},
	}
}

func TestReleaseValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Release)
		wantErr error
	}{
		{"ok", func(r *Release) {}, nil},
		{"bad version", func(r *Release) { r.Version = "x.y" }, ErrBadFeed},
		{"empty channel", func(r *Release) { r.Channel = "" }, ErrBadFeed},
		{"no artifacts", func(r *Release) { r.Artifacts = nil }, ErrBadFeed},
		{"http artifact", func(r *Release) { r.Artifacts[0].URL = "http://example.invalid/a.exe" }, ErrBadFeed},
		{"bad sha256", func(r *Release) { r.Artifacts[0].SHA256 = "abc" }, ErrBadFeed},
		{"empty name", func(r *Release) { r.Artifacts[0].Name = "  " }, ErrBadFeed},
		{"bad minimum", func(r *Release) { r.MinimumVersion = "nope" }, ErrBadFeed},
	}
	for _, c := range cases {
		rel := testRelease("0.2.0", "")
		c.mutate(&rel)
		err := rel.validate()
		if c.wantErr == nil {
			if err != nil {
				t.Errorf("%s: unexpected error %v", c.name, err)
			}
			continue
		}
		if !errors.Is(err, c.wantErr) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.wantErr)
		}
	}
}

func TestPickArtifact(t *testing.T) {
	rel := Release{Artifacts: []Artifact{
		{Name: "absorb-agent-setup-0.2.0.exe", URL: "https://x.invalid/a", SHA256: testSHA},
		{Name: "absorb-0.2.0-windows-amd64.zip", URL: "https://x.invalid/b", SHA256: testSHA},
		{Name: "absorb-0.2.0-darwin-arm64.tar.gz", URL: "https://x.invalid/c", SHA256: testSHA},
	}}
	got, err := rel.PickArtifact("windows", "amd64")
	if err != nil {
		t.Fatalf("PickArtifact: %v", err)
	}
	if got.Name != "absorb-0.2.0-windows-amd64.zip" {
		t.Fatalf("picked %q, want the windows-amd64 artifact", got.Name)
	}

	// 无平台专属命名时回退第一个 .exe
	rel2 := Release{Artifacts: []Artifact{
		{Name: "absorb-agent-setup-0.2.0.exe", URL: "https://x.invalid/a", SHA256: testSHA},
	}}
	got2, err := rel2.PickArtifact("linux", "amd64")
	if err != nil || got2.Name != "absorb-agent-setup-0.2.0.exe" {
		t.Fatalf("fallback pick = %q, err = %v", got2.Name, err)
	}

	rel3 := Release{Artifacts: []Artifact{
		{Name: "absorb-0.2.0.tar.gz", URL: "https://x.invalid/a", SHA256: testSHA},
	}}
	if _, err := rel3.PickArtifact("linux", "amd64"); !errors.Is(err, ErrNoArtifact) {
		t.Fatalf("err = %v, want ErrNoArtifact", err)
	}
}

func TestPickArtifactArchSuffix(t *testing.T) {
	rel := Release{Artifacts: []Artifact{
		{Name: "absorb-agent-setup-0.2.0-x64.exe", URL: "https://x.invalid/a", SHA256: testSHA},
	}}
	got, err := rel.PickArtifact("windows", "amd64")
	if err != nil || got.Name != "absorb-agent-setup-0.2.0-x64.exe" {
		t.Fatalf("pick = %q, err = %v", got.Name, err)
	}

	// 多架构 feed：x64/arm64 各归其位，不得退化到"回退第一个 exe"
	relMulti := Release{Artifacts: []Artifact{
		{Name: "absorb-agent-setup-0.2.0-x64.exe", URL: "https://x.invalid/a", SHA256: testSHA},
		{Name: "absorb-agent-setup-0.2.0-arm64.exe", URL: "https://x.invalid/b", SHA256: testSHA},
	}}
	gotX64, err := relMulti.PickArtifact("windows", "amd64")
	if err != nil || gotX64.Name != "absorb-agent-setup-0.2.0-x64.exe" {
		t.Fatalf("x64 pick = %q, err = %v", gotX64.Name, err)
	}
	gotArm, err := relMulti.PickArtifact("windows", "arm64")
	if err != nil || gotArm.Name != "absorb-agent-setup-0.2.0-arm64.exe" {
		t.Fatalf("arm64 pick = %q, err = %v", gotArm.Name, err)
	}

	// 显式平台专属命名（best 层）优先于 OS 中立命名（archOnly 层）
	relExplicit := Release{Artifacts: []Artifact{
		{Name: "absorb-agent-setup-0.2.0-x64.exe", URL: "https://x.invalid/a", SHA256: testSHA},
		{Name: "absorb-0.2.0-windows-amd64.exe", URL: "https://x.invalid/b", SHA256: testSHA},
	}}
	gotExplicit, err := relExplicit.PickArtifact("windows", "amd64")
	if err != nil || gotExplicit.Name != "absorb-0.2.0-windows-amd64.exe" {
		t.Fatalf("explicit pick = %q, err = %v", gotExplicit.Name, err)
	}
}

// tlsServer 起一个 https 测试服务器，并返回跳过证书校验的客户端。
func tlsServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, *http.Client) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: insecureTLS()}}
	return srv, client
}

func TestHTTPFetcher(t *testing.T) {
	t.Run("valid feed", func(t *testing.T) {
		srv, client := tlsServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(testRelease("0.2.0", "0.1.5"))
		})
		f := &HTTPFetcher{Client: client}
		rel, err := f.Fetch(context.Background(), srv.URL+"/latest.json")
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if rel.Version != "0.2.0" || rel.MinimumVersion != "0.1.5" {
			t.Fatalf("rel = %+v", rel)
		}
	})

	t.Run("404", func(t *testing.T) {
		srv, client := tlsServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		})
		f := &HTTPFetcher{Client: client}
		if _, err := f.Fetch(context.Background(), srv.URL); !errors.Is(err, ErrBadFeed) {
			t.Fatalf("err = %v, want ErrBadFeed", err)
		}
	})

	t.Run("bad json", func(t *testing.T) {
		srv, client := tlsServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("{not json"))
		})
		f := &HTTPFetcher{Client: client}
		if _, err := f.Fetch(context.Background(), srv.URL); !errors.Is(err, ErrBadFeed) {
			t.Fatalf("err = %v, want ErrBadFeed", err)
		}
	})

	t.Run("insecure url rejected", func(t *testing.T) {
		f := &HTTPFetcher{}
		if _, err := f.Fetch(context.Background(), "http://example.invalid/latest.json"); !errors.Is(err, ErrInsecureURL) {
			t.Fatalf("err = %v, want ErrInsecureURL", err)
		}
	})

	t.Run("empty url", func(t *testing.T) {
		f := &HTTPFetcher{}
		if _, err := f.Fetch(context.Background(), ""); !errors.Is(err, ErrNoFeedURL) {
			t.Fatalf("err = %v, want ErrNoFeedURL", err)
		}
	})

	t.Run("unknown fields tolerated", func(t *testing.T) {
		srv, client := tlsServer(t, func(w http.ResponseWriter, _ *http.Request) {
			body := `{"future_field":1,"extra":{"a":2},` +
				`"version":"0.3.0","channel":"stable","artifacts":[` +
				`{"name":"absorb-agent-setup-0.3.0.exe","url":"https://x.invalid/a","sha256":"` + testSHA + `"}]}`
			_, _ = w.Write([]byte(body))
		})
		f := &HTTPFetcher{Client: client}
		rel, err := f.Fetch(context.Background(), srv.URL)
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if rel.Version != "0.3.0" || len(rel.Artifacts) != 1 {
			t.Fatalf("rel = %+v", rel)
		}
	})
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// sha256Hex 计算字节串的十六进制摘要（下载校验测试用）。
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// insecureTLS 测试用：跳过证书校验的 TLS 配置（httptest 自签证书）。
func insecureTLS() *tls.Config { return &tls.Config{InsecureSkipVerify: true} } //nolint:gosec // 仅测试
