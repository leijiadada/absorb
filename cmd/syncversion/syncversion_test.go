package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtures 复刻真实文件的关键形态：wails.json 用 productVersion，
// package.json 的 version 在顶层，manifest.json 的 version 与 manifest_version /
// minimum_chrome_version 相邻（回归"误匹配 *_version 字段"的风险）。
const (
	fixWails = `{
  "$schema": "https://wails.io/schemas/config.v2.json",
  "name": "absorb-agent",
  "outputfilename": "absorb-agent-desktop",
  "info": {
    "productName": "Absorb 阅读助手",
    "productVersion": "0.1.0",
    "comments": "Agent 服务宿主 + 管理后台"
  }
}
`
	fixPkg = `{
  "name": "absorb-desktop",
  "private": true,
  "version": "0.1.0",
  "scripts": { "build": "vite build" }
}
`
	fixManifest = `{
  "manifest_version": 3,
  "name": "__MSG_extName__",
  "version": "0.1.0",
  "minimum_chrome_version": "120",
  "permissions": ["contextMenus", "nativeMessaging"]
}
`
	fixInfoJSON = `{
  "fixed": {
    "file_version": "0.1.0.0",
    "product_version": "0.1.0.0"
  },
  "info": {
    "0409": {
      "CompanyName": "absorb-agent",
      "FileDescription": "Absorb 阅读助手",
      "FileVersion": "0.1.0.0",
      "InternalName": "absorb-agent-desktop",
      "LegalCopyright": "Copyright (C) 2026 leijia",
      "OriginalFilename": "absorb-agent-desktop.exe",
      "ProductName": "Absorb 阅读助手",
      "ProductVersion": "0.1.0"
    }
  }
}
`
)

func writeFixtures(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for path, content := range map[string]string{
		filepath.Join(root, "wails.json"):                    fixWails,
		filepath.Join(root, "frontend", "package.json"):      fixPkg,
		filepath.Join(root, "extension", "manifest.json"):    fixManifest,
		filepath.Join(root, "build", "windows", "info.json"): fixInfoJSON,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return root
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestSync(t *testing.T) {
	root := writeFixtures(t)
	changed, err := Sync(root, "0.2.1")
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(changed) != 4 {
		t.Fatalf("changed = %v, want 4 files", changed)
	}

	if got := read(t, filepath.Join(root, "wails.json")); !strings.Contains(got, `"productVersion": "0.2.1"`) {
		t.Errorf("wails.json not synced:\n%s", got)
	}
	if got := read(t, filepath.Join(root, "frontend", "package.json")); !strings.Contains(got, `"version": "0.2.1"`) {
		t.Errorf("package.json not synced:\n%s", got)
	}
	manifest := read(t, filepath.Join(root, "extension", "manifest.json"))
	if !strings.Contains(manifest, `"version": "0.2.1.0"`) {
		t.Errorf("manifest version not synced to 4-segment:\n%s", manifest)
	}
	// 不得误伤名字里带 version 的其它字段
	for _, keep := range []string{`"manifest_version": 3`, `"minimum_chrome_version": "120"`} {
		if !strings.Contains(manifest, keep) {
			t.Errorf("manifest lost %s:\n%s", keep, manifest)
		}
	}
	// 其余内容必须逐字节保留（不重排 key、不改格式）
	if got := read(t, filepath.Join(root, "wails.json")); !strings.Contains(got, `"comments": "Agent 服务宿主 + 管理后台"`) {
		t.Errorf("wails.json lost unrelated fields:\n%s", got)
	}
}

// build/windows/info.json：fixed 段四段数字、展示串保留 semver，其余字段不动。
func TestSyncWindowsVersionInfo(t *testing.T) {
	root := writeFixtures(t)
	if _, err := Sync(root, "0.3.2"); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	got := read(t, filepath.Join(root, "build", "windows", "info.json"))
	for _, want := range []string{
		`"file_version": "0.3.2.0"`,
		`"product_version": "0.3.2.0"`,
		`"FileVersion": "0.3.2.0"`,
		`"ProductVersion": "0.3.2"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("info.json missing %s:\n%s", want, got)
		}
	}
	for _, keep := range []string{
		`"CompanyName": "absorb-agent"`,
		`"LegalCopyright": "Copyright (C) 2026 leijia"`,
		`"InternalName": "absorb-agent-desktop"`,
		`"OriginalFilename": "absorb-agent-desktop.exe"`,
		`"0409"`,
	} {
		if !strings.Contains(got, keep) {
			t.Errorf("info.json lost %s:\n%s", keep, got)
		}
	}
}

// 预发布版本：fixed 段仍须纯数字（goversioninfo 要求），展示串保留 semver。
func TestSyncWindowsVersionInfoPreRelease(t *testing.T) {
	root := writeFixtures(t)
	if _, err := Sync(root, "0.3.0-beta.2"); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	got := read(t, filepath.Join(root, "build", "windows", "info.json"))
	for _, want := range []string{
		`"file_version": "0.3.0.2"`,
		`"product_version": "0.3.0.2"`,
		`"FileVersion": "0.3.0.2"`,
		`"ProductVersion": "0.3.0-beta.2"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("info.json missing %s:\n%s", want, got)
		}
	}
}

// 预发布版本映射到递增的第四段（Chrome 版本必须单调递增）。
func TestSyncPreRelease(t *testing.T) {
	root := writeFixtures(t)
	if _, err := Sync(root, "0.2.0-beta.3"); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got := read(t, filepath.Join(root, "extension", "manifest.json")); !strings.Contains(got, `"version": "0.2.0.3"`) {
		t.Errorf("pre-release mapping wrong:\n%s", got)
	}
	// 非扩展产物保留原始 semver
	if got := read(t, filepath.Join(root, "wails.json")); !strings.Contains(got, `"productVersion": "0.2.0-beta.3"`) {
		t.Errorf("wails.json should keep the raw semver:\n%s", got)
	}
}

// 幂等：已是目标版本时不写文件、无改动。
func TestSyncIdempotent(t *testing.T) {
	root := writeFixtures(t)
	if _, err := Sync(root, "0.2.1"); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	before := read(t, filepath.Join(root, "wails.json"))
	changed, err := Sync(root, "0.2.1")
	if err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if len(changed) != 0 {
		t.Fatalf("second Sync changed = %v, want none", changed)
	}
	if after := read(t, filepath.Join(root, "wails.json")); after != before {
		t.Fatal("idempotent Sync rewrote the file")
	}
}

func TestSyncRejectsBadVersion(t *testing.T) {
	root := writeFixtures(t)
	for _, v := range []string{"", "abc", "0.2"} {
		if _, err := Sync(root, v); err == nil {
			t.Errorf("Sync(%q) should fail", v)
		}
	}
}

// 仓库自身的版本元数据必须与 Makefile 的默认 VERSION 一致（防再次漂移）。
func TestRepoVersionMetadataInSync(t *testing.T) {
	const want = "0.1.0" // 与 Makefile 的 VERSION ?= 保持一致
	for _, tc := range []struct{ file, field string }{
		{"wails.json", `"productVersion": "` + want + `"`},
		{"frontend/package.json", `"version": "` + want + `"`},
		{"extension/manifest.json", `"version": "` + want + `.0"`},
		{"build/windows/info.json", `"file_version": "` + want + `.0"`},
		{"build/windows/info.json", `"product_version": "` + want + `.0"`},
		{"build/windows/info.json", `"FileVersion": "` + want + `.0"`},
		{"build/windows/info.json", `"ProductVersion": "` + want + `"`},
	} {
		path := filepath.Join("..", "..", tc.file)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !strings.Contains(string(b), tc.field) {
			t.Errorf("%s 与 Makefile VERSION 不一致：缺少 %s", tc.file, tc.field)
		}
	}
}
