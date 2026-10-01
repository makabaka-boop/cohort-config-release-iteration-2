// Package semver 实现发布所需的最小语义化版本比较，
// 不引入第三方依赖：支持 major.minor.patch，可带可选的 "v" 前缀。
package semver

import (
	"fmt"
	"strconv"
	"strings"
)

// Version 是三段式语义版本。
type Version struct {
	Major int64
	Minor int64
	Patch int64
}

// String 还原为 canonical 文本。
func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// Parse 解析 "1.2" / "v1.2.3" 等写法，缺省段按 0 处理。
func Parse(s string) (Version, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Version{}, fmt.Errorf("empty version")
	}
	s = strings.TrimPrefix(s, "v")
	parts := strings.Split(s, ".")
	if len(parts) > 3 {
		return Version{}, fmt.Errorf("too many segments: %q", s)
	}
	out := Version{}
	nums := []*int64{&out.Major, &out.Minor, &out.Patch}
	for i, p := range parts {
		// 不接受 prerelease/build 后缀（如 1.2.3-rc1），避免误判兼容性。
		if p == "" || !allDigits(p) {
			return Version{}, fmt.Errorf("invalid version segment %q", p)
		}
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return Version{}, fmt.Errorf("invalid version segment %q: %w", p, err)
		}
		*nums[i] = n
	}
	return out, nil
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Compare 返回 -1/0/1，表示 a 相对 b 的新旧。
func Compare(a, b Version) int {
	switch {
	case a.Major != b.Major:
		return sign(a.Major - b.Major)
	case a.Minor != b.Minor:
		return sign(a.Minor - b.Minor)
	case a.Patch != b.Patch:
		return sign(a.Patch - b.Patch)
	default:
		return 0
	}
}

// GTE 报告 a >= b，即客户端版本是否满足最低版本要求。
func GTE(a, b Version) bool { return Compare(a, b) >= 0 }

func sign(n int64) int {
	if n < 0 {
		return -1
	}
	return 1
}
