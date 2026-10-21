package pyrt

import (
	_ "embed"
	"fmt"
	"strings"
	"sync"
)

//go:embed requirements.lock
var requirementsLock []byte

// lockSpec 是 requirements.lock 里和安装有关的部分。
type lockSpec struct {
	min          Version
	maxExclusive *Version
	packages     []lockedPkg
}

type lockedPkg struct {
	Name    string
	Version string
	Hashes  []string
}

func (s lockSpec) supports(v Version) bool {
	if v.cmp(s.min) < 0 {
		return false
	}
	if s.maxExclusive != nil && v.cmp(*s.maxExclusive) >= 0 {
		return false
	}
	return true
}

func (s lockSpec) rangeString() string {
	text := ">=" + fmt.Sprintf("%d.%d", s.min.Major, s.min.Minor)
	if s.maxExclusive != nil {
		text += fmt.Sprintf(",<%d.%d", s.maxExclusive.Major, s.maxExclusive.Minor)
	}
	return text
}

var (
	lockOnce   sync.Once
	lockCached lockSpec
	lockErr    error
)

func embeddedLock() (lockSpec, error) {
	lockOnce.Do(func() {
		lockCached, lockErr = parseLock(requirementsLock)
	})
	return lockCached, lockErr
}

func parseLock(raw []byte) (lockSpec, error) {
	var spec lockSpec
	var logical strings.Builder
	cont := false
	seenDirective := false
	seen := map[string]struct{}{}
	for _, line := range strings.Split(string(raw), "\n") {
		trim := strings.TrimSpace(line)
		if cont {
			if trim == "" {
				return lockSpec{}, fmt.Errorf("续行后面是空行")
			}
			logical.WriteByte(' ')
			logical.WriteString(strings.TrimSuffix(trim, "\\"))
			cont = strings.HasSuffix(trim, "\\")
			if cont {
				continue
			}
			if err := spec.addReq(logical.String(), seen); err != nil {
				return lockSpec{}, err
			}
			logical.Reset()
			continue
		}
		if trim == "" || strings.HasPrefix(trim, "#") {
			if strings.HasPrefix(trim, "# lantern-python:") {
				if seenDirective {
					return lockSpec{}, fmt.Errorf("重复的 lantern-python")
				}
				seenDirective = true
				dir := strings.TrimSpace(strings.TrimPrefix(trim, "# lantern-python:"))
				min, max, err := parsePythonRange(dir)
				if err != nil {
					return lockSpec{}, err
				}
				spec.min = min
				spec.maxExclusive = max
			}
			continue
		}
		if strings.HasSuffix(trim, "\\") {
			logical.Reset()
			logical.WriteString(strings.TrimSuffix(trim, "\\"))
			cont = true
			continue
		}
		if err := spec.addReq(trim, seen); err != nil {
			return lockSpec{}, err
		}
	}
	if cont {
		return lockSpec{}, fmt.Errorf("锁定文件以续行结束")
	}
	if !seenDirective {
		return lockSpec{}, fmt.Errorf("缺少 # lantern-python")
	}
	if len(spec.packages) == 0 {
		return lockSpec{}, fmt.Errorf("没有依赖")
	}
	if !hasPkg(spec, "pandas") || !hasPkg(spec, "matplotlib") {
		return lockSpec{}, fmt.Errorf("锁定文件必须包含 pandas 和 matplotlib")
	}
	return spec, nil
}

func hasPkg(spec lockSpec, name string) bool {
	for _, pkg := range spec.packages {
		if pkg.Name == name {
			return true
		}
	}
	return false
}

func (s *lockSpec) addReq(logical string, seen map[string]struct{}) error {
	fields := strings.Fields(logical)
	if len(fields) < 2 {
		return fmt.Errorf("依赖缺少哈希: %s", fieldsFirst(fields))
	}
	name, ver, ok := strings.Cut(fields[0], "==")
	if !ok || !validName(name) || ver == "" || strings.ContainsAny(ver, "*<>!") {
		return fmt.Errorf("依赖必须用 == 精确固定: %s", fields[0])
	}
	if _, dup := seen[name]; dup {
		return fmt.Errorf("重复的依赖 %s", name)
	}
	hashes := make([]string, 0, len(fields)-1)
	for _, field := range fields[1:] {
		sum, ok := strings.CutPrefix(field, "--hash=sha256:")
		if !ok || !validSHA256(sum) {
			return fmt.Errorf("%s 含有非法哈希", name)
		}
		hashes = append(hashes, strings.ToLower(sum))
	}
	seen[name] = struct{}{}
	s.packages = append(s.packages, lockedPkg{Name: name, Version: ver, Hashes: hashes})
	return nil
}

func fieldsFirst(fields []string) string {
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func validName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func validSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// parsePythonRange 只接受本仓库锁文件使用的写法，例如 >=3.10,<3.14。
func parsePythonRange(s string) (Version, *Version, error) {
	if s == "" {
		return Version{}, nil, fmt.Errorf("lantern-python 为空")
	}
	var min Version
	var max *Version
	var sawMin bool
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		switch {
		case strings.HasPrefix(part, ">="):
			if sawMin {
				return Version{}, nil, fmt.Errorf("重复的 >=")
			}
			v, err := parseVersionToken(strings.TrimPrefix(part, ">="))
			if err != nil {
				return Version{}, nil, err
			}
			min = v
			sawMin = true
		case strings.HasPrefix(part, "<"):
			if max != nil {
				return Version{}, nil, fmt.Errorf("重复的 <")
			}
			v, err := parseVersionToken(strings.TrimPrefix(part, "<"))
			if err != nil {
				return Version{}, nil, err
			}
			cp := v
			max = &cp
		default:
			return Version{}, nil, fmt.Errorf("无法识别的范围 %q", part)
		}
	}
	if !sawMin {
		return Version{}, nil, fmt.Errorf("lantern-python 缺少 >=")
	}
	if max != nil && min.cmp(*max) >= 0 {
		return Version{}, nil, fmt.Errorf("版本范围为空")
	}
	return min, max, nil
}
