package upgrade

import (
	"fmt"
	"runtime"
	"strings"
	"time"
)

// Artifact 一个可下载产物。
type Artifact struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"` // 小写十六进制
	Size   int64  `json:"size,omitempty"`
}

// Release feed 描述的一个版本（latest.json 的解析目标）。
type Release struct {
	Version        string     `json:"version"`
	Channel        string     `json:"channel"`
	PublishedAt    time.Time  `json:"published_at"`
	MinimumVersion string     `json:"minimum_version,omitempty"` // 低于此版本必须升级
	NotesURL       string     `json:"notes_url,omitempty"`
	Artifacts      []Artifact `json:"artifacts"`
}

// validate 必填字段与安全约束：版本可解析、通道非空、至少一个 https 产物且 SHA256 合法。
func (r *Release) validate() error {
	if _, err := ParseVersion(r.Version); err != nil {
		return fmt.Errorf("%w: version: %v", ErrBadFeed, err)
	}
	if strings.TrimSpace(r.Channel) == "" {
		return fmt.Errorf("%w: channel is empty", ErrBadFeed)
	}
	if len(r.Artifacts) == 0 {
		return fmt.Errorf("%w: no artifacts", ErrBadFeed)
	}
	for _, a := range r.Artifacts {
		if strings.TrimSpace(a.Name) == "" || !isHTTPS(a.URL) {
			return fmt.Errorf("%w: artifact %q has empty name or non-https url", ErrBadFeed, a.Name)
		}
		if !isSHA256Hex(a.SHA256) {
			return fmt.Errorf("%w: artifact %q has bad sha256", ErrBadFeed, a.Name)
		}
	}
	if r.MinimumVersion != "" {
		if _, err := ParseVersion(r.MinimumVersion); err != nil {
			return fmt.Errorf("%w: minimum_version: %v", ErrBadFeed, err)
		}
	}
	return nil
}

// otherOSNames 显式标注其他平台的 OS 标记；名字命中任一则不算 OS 中立。
var otherOSNames = []string{"darwin", "macos", "linux", "android", "ios", "freebsd"}

// normalizeArtifactName 小写化并归一架构别名：安装包命名用人类习惯的 x64，匹配按 GOARCH（amd64）。
func normalizeArtifactName(name string) string {
	return strings.ReplaceAll(strings.ToLower(name), "x64", "amd64")
}

func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// PickArtifact 按 goos/goarch 选安装包，优先级从高到低：
//  1. 名字同时含 goos 与 goarch；
//  2. OS 中立（名字无任何 OS 标记）但含 goarch——如 absorb-agent-setup-0.1.0-x64.exe，
//     文件名不带 windows 但只发 Windows，x64 经 normalizeArtifactName 归一；
//  3. 仅含 goos；
//  4. 回退第一个 .exe（无架构后缀的旧命名）。
//
// 多架构 feed（同时挂 x64/arm64）依赖第 2 层按 goarch 区分，不能退化到回退层。
func (r *Release) PickArtifact(goos, goarch string) (*Artifact, error) {
	best, archOnly, goosOnly, fallback := -1, -1, -1, -1
	for i, a := range r.Artifacts {
		name := normalizeArtifactName(a.Name)
		hasOS := strings.Contains(name, goos)
		hasArch := strings.Contains(name, goarch)
		osNeutral := !hasOS && !containsAny(name, otherOSNames)
		switch {
		case hasOS && hasArch:
			if best < 0 {
				best = i
			}
		case osNeutral && hasArch:
			if archOnly < 0 {
				archOnly = i
			}
		case hasOS:
			if goosOnly < 0 {
				goosOnly = i
			}
		case strings.HasSuffix(name, ".exe") && fallback < 0:
			fallback = i
		}
	}
	for _, idx := range []int{best, archOnly, goosOnly, fallback} {
		if idx >= 0 {
			a := r.Artifacts[idx]
			return &a, nil
		}
	}
	return nil, fmt.Errorf("%w: %s/%s", ErrNoArtifact, goos, goarch)
}

// isHTTPS 仅接受 https（升级链路不降级到 http）。
func isHTTPS(u string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(u)), "https://")
}

// isSHA256Hex 64 位十六进制（大小写均可）。
func isSHA256Hex(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

// platform 当前 GOOS/GOARCH（PickArtifact 的默认入参）。
func platform() (string, string) { return runtime.GOOS, runtime.GOARCH }
