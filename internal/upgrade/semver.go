package upgrade

import (
	"fmt"
	"strconv"
	"strings"
)

// Version 是内部 semver 表示（不含构建元数据；预-release 保留原文）。
//
// 为什么内部实现而不引 golang.org/x/mod/semver：
//   - 避免为一个 60 行的比较逻辑新增直接依赖；
//   - 需要统一处理本项目真实会出现的输入："dev"、"0.0.0-dev-<sha>"、
//     "-dirty"，以及扩展 manifest 的四段版本（1.2.3.4）。
type Version struct {
	Major, Minor, Patch int
	Pre                 string // 预-release 标识（如 "beta.1"）；空 = 正式版
	Dev                 bool   // 开发构建：不参与版本比较
}

// ParseVersion 解析版本串。兼容：前导 v/V、四段（取前三段）、构建元数据（+xxx 忽略）、
// dev 形态（"dev"、含 "-dirty"、含 "-dev"）。预-release 段按 semver 规则校验。
func ParseVersion(s string) (Version, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return Version{}, fmt.Errorf("%w: empty", ErrBadVersion)
	}
	low := strings.ToLower(raw)
	if low == "dev" || strings.Contains(low, "-dirty") || strings.Contains(low, "-dev") {
		return Version{Dev: true}, nil
	}
	v := strings.TrimPrefix(strings.TrimPrefix(raw, "v"), "V")
	if i := strings.IndexByte(v, '+'); i >= 0 { // 丢弃构建元数据
		v = v[:i]
	}
	core, pre := v, ""
	hasDash := false
	if i := strings.IndexByte(v, '-'); i >= 0 {
		core, pre, hasDash = v[:i], v[i+1:], true
	}
	parts := strings.Split(core, ".")
	if len(parts) < 3 || len(parts) > 4 {
		return Version{}, fmt.Errorf("%w: %q (want MAJOR.MINOR.PATCH)", ErrBadVersion, s)
	}
	nums := make([]int, 3)
	for i := range nums {
		n, err := strconv.Atoi(parts[i])
		if err != nil || n < 0 {
			return Version{}, fmt.Errorf("%w: %q", ErrBadVersion, s)
		}
		nums[i] = n
	}
	if len(parts) == 4 { // 第四段（扩展 manifest 版本）必须为数字
		if _, err := strconv.Atoi(parts[3]); err != nil {
			return Version{}, fmt.Errorf("%w: %q", ErrBadVersion, s)
		}
	}
	if pre != "" && !validPreRelease(pre) {
		return Version{}, fmt.Errorf("%w: bad pre-release %q", ErrBadVersion, pre)
	}
	if hasDash && pre == "" {
		// "1.0.0-"：有预-release 分隔符但标识为空，semver 视为非法
		return Version{}, fmt.Errorf("%w: empty pre-release %q", ErrBadVersion, s)
	}
	return Version{Major: nums[0], Minor: nums[1], Patch: nums[2], Pre: pre}, nil
}

// validPreRelease 预-release 各标识非空，且仅含 [0-9A-Za-z-]。
func validPreRelease(pre string) bool {
	for _, id := range strings.Split(pre, ".") {
		if id == "" {
			return false
		}
		for _, r := range id {
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '-') {
				return false
			}
		}
	}
	return true
}

// Compare 返回 -1/0/1（a 小于/等于/大于 b）。Dev 版本调用方应先过滤，不在此比较。
// 规则：主次修订号数值比较；正式版 > 预发布；预发布按标识逐个比较
// （数字标识数值比较且数字 < 字母；前缀相等时更短的更小）。
func (a Version) Compare(b Version) int {
	if c := cmpInt(a.Major, b.Major); c != 0 {
		return c
	}
	if c := cmpInt(a.Minor, b.Minor); c != 0 {
		return c
	}
	if c := cmpInt(a.Patch, b.Patch); c != 0 {
		return c
	}
	switch {
	case a.Pre == b.Pre:
		return 0
	case a.Pre == "":
		return 1
	case b.Pre == "":
		return -1
	default:
		return comparePre(a.Pre, b.Pre)
	}
}

func comparePre(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		if c := compareIdent(as[i], bs[i]); c != 0 {
			return c
		}
	}
	return cmpInt(len(as), len(bs))
}

func compareIdent(a, b string) int {
	an, aErr := strconv.Atoi(a)
	bn, bErr := strconv.Atoi(b)
	switch {
	case aErr == nil && bErr == nil:
		return cmpInt(an, bn)
	case aErr == nil: // 数字标识 < 字母标识
		return -1
	case bErr == nil:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// String 还原为 semver 串（开发构建返回 "dev"）。
func (v Version) String() string {
	if v.Dev {
		return "dev"
	}
	s := strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor) + "." + strconv.Itoa(v.Patch)
	if v.Pre != "" {
		s += "-" + v.Pre
	}
	return s
}

// ExtensionVersion 把 semver 映射为 Chrome 扩展 manifest 的四段版本
// （如 0.2.0 → "0.2.0.0"，0.2.0-beta.1 → "0.2.0.1"）。预-release 用递增的
// 第四段表达，保证扩展版本随发布单调递增；无法解析时回退 "0.0.0.0"。
func ExtensionVersion(semver string) string {
	v, err := ParseVersion(semver)
	if err != nil || v.Dev {
		return "0.0.0.0"
	}
	fourth := 0
	if v.Pre != "" {
		for _, id := range strings.Split(v.Pre, ".") {
			if n, err := strconv.Atoi(id); err == nil {
				fourth = n
				break
			}
		}
		if fourth == 0 {
			fourth = 1 // 预-release 至少为 1，避免与正式版同版本号
		}
	}
	return strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor) + "." + strconv.Itoa(v.Patch) + "." + strconv.Itoa(fourth)
}
