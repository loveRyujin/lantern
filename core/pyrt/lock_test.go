package pyrt

import (
	"strings"
	"testing"
)

func TestEmbeddedLock(t *testing.T) {
	spec, err := parseLock(requirementsLock)
	if err != nil {
		t.Fatal(err)
	}
	if spec.rangeString() != ">=3.10,<3.14" {
		t.Fatalf("范围 = %s", spec.rangeString())
	}
	if !spec.supports(Version{Major: 3, Minor: 10}) || !spec.supports(Version{Major: 3, Minor: 13, Micro: 9}) {
		t.Fatal("3.10 和 3.13 应该在锁定范围内")
	}
	if spec.supports(Version{Major: 3, Minor: 9, Micro: 9}) || spec.supports(Version{Major: 3, Minor: 14}) {
		t.Fatal("3.9 和 3.14 不应该在锁定范围内")
	}
	want := map[string]string{
		"pandas":     "2.3.3",
		"matplotlib": "3.10.9",
		"numpy":      "2.2.6",
	}
	got := map[string]lockedPkg{}
	for _, pkg := range spec.packages {
		if len(pkg.Hashes) == 0 {
			t.Fatalf("%s 没有哈希", pkg.Name)
		}
		for _, hash := range pkg.Hashes {
			if !validSHA256(hash) {
				t.Fatalf("%s 哈希 %s 不合法", pkg.Name, hash)
			}
		}
		got[pkg.Name] = pkg
	}
	for name, ver := range want {
		pkg, ok := got[name]
		if !ok || pkg.Version != ver {
			t.Fatalf("%s = %+v，想要 %s", name, pkg, ver)
		}
	}
	if !strings.Contains(string(requirementsLock), "--hash=sha256:") {
		t.Fatal("锁文件里没有 sha256")
	}
}

func TestParseLockRejectsBadFiles(t *testing.T) {
	cases := []string{
		"",
		"# lantern-python: >=3.10,<3.14\n",
		"# lantern-python: >=3.10,<3.14\npandas==2.3.3\n",
		"# lantern-python: >=3.10,<3.14\n# lantern-python: >=3.10,<3.14\npandas==2.3.3 \\\n    --hash=sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n",
		"# lantern-python: >=3.14,<3.10\npandas==2.3.3 \\\n    --hash=sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n",
		"pandas==2.3.3 \\\n    --hash=sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n",
	}
	for _, raw := range cases {
		if _, err := parseLock([]byte(raw)); err == nil {
			t.Fatalf("应该拒绝:\n%s", raw)
		}
	}
}

func TestSearchPaths(t *testing.T) {
	darwin := searchPaths("darwin")
	if len(darwin) < 2 || darwin[0] != "/opt/homebrew/bin/python3" {
		t.Fatalf("darwin = %v", darwin)
	}
	linux := searchPaths("linux")
	if len(linux) != 2 || linux[0] != "/usr/local/bin/python3" || linux[1] != "/usr/bin/python3" {
		t.Fatalf("linux = %v", linux)
	}
	if searchPaths("windows") != nil {
		t.Fatalf("windows = %v", searchPaths("windows"))
	}
}
