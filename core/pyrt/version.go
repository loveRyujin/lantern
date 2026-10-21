package pyrt

import (
	"fmt"
	"strconv"
	"strings"
)

// Version 是解释器的 sys.version_info 前三段。
type Version struct {
	Major int
	Minor int
	Micro int
}

// String 返回 major.minor.micro。
func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Micro)
}

// AtLeast 报告 v 是否不低于 major.minor。补丁号不参与比较。
func (v Version) AtLeast(major, minor int) bool {
	if v.Major != major {
		return v.Major > major
	}
	return v.Minor >= minor
}

func (v Version) cmp(o Version) int {
	if v.Major != o.Major {
		return cmpInt(v.Major, o.Major)
	}
	if v.Minor != o.Minor {
		return cmpInt(v.Minor, o.Minor)
	}
	return cmpInt(v.Micro, o.Micro)
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

func parseVersionToken(s string) (Version, error) {
	parts := strings.Split(s, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return Version{}, fmt.Errorf("版本 %q 不是 major.minor", s)
	}
	nums := [3]int{}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 || (len(part) > 1 && part[0] == '0') {
			return Version{}, fmt.Errorf("版本 %q 含有非法段", s)
		}
		nums[i] = n
	}
	return Version{Major: nums[0], Minor: nums[1], Micro: nums[2]}, nil
}
