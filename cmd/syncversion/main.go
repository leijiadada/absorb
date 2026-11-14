// Command syncversion 把构建版本同步到各产物的版本元数据。
//
// 背景：版本号曾散落在 5 处（Makefile VERSION / wails.json / extension/manifest.json /
// frontend/package.json / ldflags），靠手工同步必然漏。真机升级时就暴露出PE 版本资源
// 停在 0.1.0 而实际构建是 0.1.1-test 的问题（Windows 文件属性与任何读
// VersionInfo.ProductVersion 的工具都会看到旧版本）。
//
// 本命令由 Makefile 的 sync-version 目标在构建前调用（见 Makefile），以 Makefile 的
// VERSION 为唯一权威：
//
//	wails.json                info.productVersion → 原样
//	frontend/package.json     version             → 原样
//	extension/manifest.json   version             → 四段映射（Chrome 只接受 1-4 段整数）
//	build/windows/info.json   file_version / product_version / FileVersion → 四段映射
//	                           ProductVersion      → 原样
//
// 最后一项容易被漏掉：该文件被 git 跟踪且**优先级高于 wails.json**——Wails 的 Windows
// 版本资源直接用它，不同步就会让所有产物的 PE 版本永远停在旧值（真机验证过：
// wails.json 已是 0.1.1-test，装出来的 exe 的 ProductVersion 仍是 0.1.0）。
// 其中的 fixed.* 必须是 x.x.x.x 纯数字（goversioninfo 要求），故用四段映射；
// info.*.ProductVersion 是展示串，保留原始 semver。
//
// 只做定点文本替换，文件其余内容逐字节保持不变（不重排 JSON key、不改格式），
// 因此同步不会产生无关 diff。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"absorb-agent/internal/upgrade"
)

var (
	reProductVersion = regexp.MustCompile(`(?m)^(\s*"productVersion"\s*:\s*")([^"]*)(")`)
	reVersion        = regexp.MustCompile(`(?m)^(\s*"version"\s*:\s*")([^"]*)(")`)
	// build/windows/info.json：fixed 段必须四段纯数字，info 段的展示串保留 semver。
	// 注意该文件用 PascalCase 的 "ProductVersion"/"FileVersion"，与 wails.json 的
	// camelCase "productVersion" 不是同一个字段，需分别匹配。
	reFixedFileVersion    = regexp.MustCompile(`(?m)^(\s*"file_version"\s*:\s*")([^"]*)(")`)
	reFixedProductVersion = regexp.MustCompile(`(?m)^(\s*"product_version"\s*:\s*")([^"]*)(")`)
	reFileVersion         = regexp.MustCompile(`(?m)^(\s*"FileVersion"\s*:\s*")([^"]*)(")`)
	reDisplayProductVer   = regexp.MustCompile(`(?m)^(\s*"ProductVersion"\s*:\s*")([^"]*)(")`)
)

// Sync 把 version 同步到 root 下的产物元数据，返回「文件 → 旧值→新值」的改动摘要。
// 已是目标版本时不写文件（无副作用），可安全重复执行。
func Sync(root, version string) (map[string]string, error) {
	if strings.TrimSpace(version) == "" {
		return nil, fmt.Errorf("version 不能为空")
	}
	if _, err := upgrade.ParseVersion(version); err != nil {
		return nil, fmt.Errorf("version %q 无法解析为 semver: %w", version, err)
	}

	changed := map[string]string{}
	four := upgrade.ExtensionVersion(version) // 四段数字版本（goversioninfo / Chrome 要求）
	edits := []struct {
		file string
		re   *regexp.Regexp
		want string
	}{
		{filepath.Join("wails.json"), reProductVersion, version},
		{filepath.Join("frontend", "package.json"), reVersion, version},
		// Chrome 扩展 manifest 的 version 只能是 1-4 段点分整数，预发布用递增的第四段表达
		{filepath.Join("extension", "manifest.json"), reVersion, four},
		// Windows PE 版本资源（Wails 优先用该文件而非 wails.json）
		{filepath.Join("build", "windows", "info.json"), reFixedFileVersion, four},
		{filepath.Join("build", "windows", "info.json"), reFixedProductVersion, four},
		{filepath.Join("build", "windows", "info.json"), reFileVersion, four},
		{filepath.Join("build", "windows", "info.json"), reDisplayProductVer, version},
	}
	// 同一文件多处替换需顺序执行，按文件聚合结果
	perFile := map[string][]string{}
	for _, e := range edits {
		path := filepath.Join(root, e.file)
		old, err := patchField(path, e.re, e.want)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.file, err)
		}
		if old != e.want {
			perFile[e.file] = append(perFile[e.file], fmt.Sprintf("%s → %s", old, e.want))
		}
	}
	for file, chs := range perFile {
		changed[file] = strings.Join(chs, "; ")
	}
	return changed, nil
}

// patchField 用正则替换文件中第一个匹配字段的值，返回旧值；未变化则不写文件。
// 捕获组：1=字段前缀（含开引号）、2=版本值、3=收引号；只替换组 2，其余逐字节保留。
func patchField(path string, re *regexp.Regexp, want string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	loc := re.FindSubmatchIndex(raw)
	if len(loc) < 6 {
		return "", fmt.Errorf("未找到待同步的版本字段")
	}
	old := string(raw[loc[4]:loc[5]])
	if old == want {
		return old, nil
	}
	out := make([]byte, 0, len(raw)+16)
	out = append(out, raw[:loc[4]]...)
	out = append(out, want...)
	out = append(out, raw[loc[5]:]...)
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return "", err
	}
	return old, nil
}

func main() {
	version := flag.String("version", "", "构建版本（Makefile 的 VERSION，如 0.1.1）")
	root := flag.String("root", ".", "仓库根目录")
	flag.Parse()

	if *version == "" {
		fmt.Fprintln(os.Stderr, "用法: syncversion -version <版本> [-root <仓库根>]")
		os.Exit(2)
	}
	changed, err := Sync(*root, *version)
	if err != nil {
		fmt.Fprintf(os.Stderr, "syncversion: %v\n", err)
		os.Exit(1)
	}
	if len(changed) == 0 {
		fmt.Printf("[sync-version] 各产物版本均已为 %s，无需改动\n", *version)
		return
	}
	for file, ch := range changed {
		fmt.Printf("[sync-version] %s: %s\n", file, ch)
	}
}
