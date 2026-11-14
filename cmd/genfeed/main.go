package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"absorb-agent/internal/upgrade"
)

// genfeed 由构建出的安装包生成 releases/latest.json：sha256/size 对安装包实算，
// 下载 url 按 Releases 命名约定（/v<版本>/<文件名>）拼出，channel/minimum_version/
// notes_url 沿用现有 feed 的取值。支持 -exe 重复传入多架构安装包（如 x64/arm64），
// 产物合并写入同一 artifacts 数组，供客户端按架构选包。make installer 出包后可本地
// 调用；CI 发布流水线（.github/workflows/release.yml）在 ISCC 出包后自动调用并随
// Release 提交回 master。
const defaultNotesURL = "https://github.com/leijiadada/absorb/releases"

// exeList 可重复的 -exe 参数。
type exeList []string

func (e *exeList) String() string { return strings.Join(*e, ",") }
func (e *exeList) Set(v string) error {
	*e = append(*e, v)
	return nil
}

func main() {
	os.Exit(run())
}

func run() int {
	version := flag.String("version", "", "新版本号（MAJOR.MINOR.PATCH）")
	root := flag.String("root", ".", "仓库根目录")
	var exes exeList
	flag.Var(&exes, "exe", "安装包路径，可重复传入多架构产物（默认 build/bin/absorb-agent-setup-<version>-x64.exe）")
	out := flag.String("out", "", "feed 输出路径（默认 <root>/releases/latest.json）")
	urlBase := flag.String("url-base", "https://github.com/leijiadada/absorb/releases/download", "Releases 下载地址前缀")
	channel := flag.String("channel", "", "覆盖 channel（默认沿用现有 feed，缺省 stable）")
	minVersion := flag.String("minimum-version", "", "覆盖 minimum_version（默认沿用现有 feed）")
	notesURL := flag.String("notes-url", "", "覆盖 notes_url（默认沿用现有 feed）")
	force := flag.Bool("force", false, "允许用更低版本覆盖现有 feed")
	flag.Parse()

	if *version == "" {
		return fail(errors.New("-version 必填"))
	}
	v, err := upgrade.ParseVersion(*version)
	if err != nil {
		return fail(fmt.Errorf("version 非法: %w", err))
	}
	if v.Dev {
		return fail(errors.New("feed 不接受 dev 版本号"))
	}

	exePaths := []string(exes)
	if len(exePaths) == 0 {
		exePaths = []string{filepath.Join(*root, "build", "bin", fmt.Sprintf("absorb-agent-setup-%s-x64.exe", *version))}
	}
	outPath := *out
	if outPath == "" {
		outPath = filepath.Join(*root, "releases", "latest.json")
	}

	cur, hasCur, err := loadFeed(outPath)
	if err != nil {
		return fail(err)
	}
	// 防降级：默认 VERSION=0.1.0 的本地构建不得覆盖线上更高的 feed 版本
	if hasCur && !*force {
		if ov, err := upgrade.ParseVersion(cur.Version); err == nil && older(v, ov) {
			return fail(fmt.Errorf("拒绝把 feed 从 %s 降级为 %s（-force 可解除）", cur.Version, *version))
		}
	}

	ch := *channel
	if ch == "" && hasCur {
		ch = cur.Channel
	}
	if ch == "" {
		ch = "stable"
	}
	minv := *minVersion
	if minv == "" && hasCur {
		minv = cur.MinimumVersion
	}
	notes := *notesURL
	if notes == "" && hasCur {
		notes = cur.NotesURL
	}
	if notes == "" {
		notes = defaultNotesURL
	}

	url := strings.TrimRight(*urlBase, "/") + "/v" + *version + "/"
	if !strings.HasPrefix(strings.ToLower(url), "https://") {
		return fail(errors.New("url-base 必须是 https"))
	}

	artifacts := make([]upgrade.Artifact, 0, len(exePaths))
	for _, exePath := range exePaths {
		name := filepath.Base(exePath)
		sum, size, err := hashFile(exePath)
		if err != nil {
			return fail(fmt.Errorf("读取安装包 %s: %w", exePath, err))
		}
		artifacts = append(artifacts, upgrade.Artifact{
			Name:   name,
			URL:    url + name,
			SHA256: sum,
			Size:   size,
		})
	}

	rel := upgrade.Release{
		Version:        *version,
		Channel:        ch,
		PublishedAt:    time.Now().UTC().Truncate(time.Second),
		MinimumVersion: minv,
		NotesURL:       notes,
		Artifacts:      artifacts,
	}
	data, err := json.MarshalIndent(rel, "", "  ")
	if err != nil {
		return fail(err)
	}
	data = append(data, '\n')

	// 终检：按客户端同款逻辑反解并选包，确保生成的 feed 客户端一定可用
	var check upgrade.Release
	if err := json.Unmarshal(data, &check); err != nil {
		return fail(fmt.Errorf("feed 自检失败: %w", err))
	}
	if _, err := check.PickArtifact("windows", "amd64"); err != nil {
		return fail(fmt.Errorf("客户端无法从生成的 feed 选出产物: %w", err))
	}

	if err := os.WriteFile(outPath, data, 0o644); err != nil {
		return fail(err)
	}
	fmt.Printf("feed 已生成: %s (version=%s artifacts=%d)\n", outPath, *version, len(artifacts))
	return 0
}

// loadFeed 读取现有 feed；文件不存在视为首次生成。
func loadFeed(path string) (*upgrade.Release, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var rel upgrade.Release
	if err := json.Unmarshal(data, &rel); err != nil {
		return nil, false, fmt.Errorf("解析现有 %s: %w", path, err)
	}
	return &rel, true, nil
}

// hashFile 计算文件 SHA256（小写十六进制）与字节数。
func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// older 按主/次/修订号判断 a 是否低于 b（仅作防降级闸，不处理预-release 细则）。
func older(a, b upgrade.Version) bool {
	if a.Major != b.Major {
		return a.Major < b.Major
	}
	if a.Minor != b.Minor {
		return a.Minor < b.Minor
	}
	return a.Patch < b.Patch
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "genfeed:", err)
	return 1
}
