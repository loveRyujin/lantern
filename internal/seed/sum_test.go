package seed_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

func TestOutsideCallGoSumMatchesModule(t *testing.T) {
	root := lanternRoot(t)
	parentPath, err := filepath.EvalSymlinks(filepath.Join(root, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	parent := readSum(t, parentPath)
	for _, rel := range []string{
		filepath.Join("internal", "seed", "testdata", "outsidecall", "go.sum"),
		filepath.Join("internal", "seed", "testdata", "subcall", "go.sum"),
	} {
		childPath, err := filepath.EvalSymlinks(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		if err := sameSumEntries(parent, readSum(t, childPath)); err != nil {
			t.Fatalf("%s: %v", filepath.ToSlash(rel), err)
		}
	}
}

func TestMutatedGoSumIsRejected(t *testing.T) {
	root := lanternRoot(t)
	parent := readSum(t, filepath.Join(root, "go.sum"))
	original, err := os.ReadFile(filepath.Join(root, "internal", "seed", "testdata", "outsidecall", "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	mutated := []byte(strings.Replace(string(original), "h1:", "h1:ff", 1))
	if string(mutated) == string(original) {
		t.Fatal("没有改到哈希")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "go.sum")
	if err := os.WriteFile(path, mutated, 0o644); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := sameSumEntries(parent, readSum(t, resolved)); err == nil {
		t.Fatal("改过的 go.sum 不应算一致")
	}
}

func TestOutsideCallPackageLoads(t *testing.T) {
	root := lanternRoot(t)
	dir, err := filepath.EvalSymlinks(filepath.Join(root, "internal", "seed", "testdata", "outsidecall"))
	if err != nil {
		t.Fatal(err)
	}
	pkgs, err := packages.Load(&packages.Config{
		Mode: packages.NeedName | packages.NeedTypes | packages.NeedDeps | packages.NeedImports,
		Dir:  dir,
		Env:  envWithCGODisabled(),
	}, "./...")
	if err != nil {
		t.Fatalf("加载失败不能当成通过: %v", err)
	}
	if len(pkgs) == 0 {
		t.Fatal("没有加载到包")
	}
	if msg := packageErrors(pkgs); msg != "" {
		t.Fatalf("加载失败不能当成通过:\n%s", msg)
	}
}

func readSum(t *testing.T, path string) map[string]string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, hash, ok := strings.Cut(line, " h1:")
		if !ok {
			t.Fatalf("无法解析 %s 里的 %q", path, line)
		}
		out[key] = hash
	}
	if len(out) == 0 {
		t.Fatalf("%s 是空的", path)
	}
	return out
}

func sameSumEntries(parent, child map[string]string) error {
	for key, hash := range child {
		got, ok := parent[key]
		if !ok || got != hash {
			return &sumMismatch{key: key, child: hash, parent: got, missing: !ok}
		}
	}
	return nil
}

type sumMismatch struct {
	key     string
	child   string
	parent  string
	missing bool
}

func (e *sumMismatch) Error() string {
	if e.missing {
		return e.key + " 不在主模块 go.sum 里"
	}
	return e.key + " 的哈希和主模块不一致"
}
