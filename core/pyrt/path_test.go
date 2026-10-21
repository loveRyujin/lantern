package pyrt

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loveRyujin/lantern/core/errcode"
)

func TestEnsureRejectsPyenvSymlinkOutsideRoot(t *testing.T) {
	installFakePy(t, "3.12.1")
	outside := t.TempDir()
	plantFile(t, filepath.Join(outside, "venv", "precious.txt"), "keep-venv")
	plantFile(t, filepath.Join(outside, "precious.txt"), "keep-root")
	before := snapshotTree(t, outside)

	lexical, real, ancestor := rootUnderLink(t)
	link := filepath.Join(real, "pyenv")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	_, err := Config{Root: lexical, SearchPaths: []string{}}.Ensure(context.Background())
	assertEscapeAt(t, err, link, ancestor)
	if got := snapshotTree(t, outside); got != before {
		t.Fatalf("Root 外的文件被改动了\n之前:\n%s\n之后:\n%s", before, got)
	}
}

func TestEnsureRejectsIntermediateSymlink(t *testing.T) {
	installFakePy(t, "3.12.1")
	outside := t.TempDir()
	plantFile(t, filepath.Join(outside, "precious.txt"), "keep")
	before := snapshotTree(t, outside)

	lexical, real, ancestor := rootUnderLink(t)
	if err := os.Mkdir(filepath.Join(real, "pyenv"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 链接在已解析 Root 内部，是 pyenv 与更下面路径之间的那一层，不是祖先目录。
	link := filepath.Join(real, "pyenv", "venv")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	_, err := Config{Root: lexical, SearchPaths: []string{}}.Ensure(context.Background())
	assertEscapeAt(t, err, link, ancestor)
	if got := snapshotTree(t, outside); got != before {
		t.Fatalf("Root 内符号链接指向的目录被改动了\n之前:\n%s\n之后:\n%s", before, got)
	}
}

func TestEnsureTOCTOUPyenvReplacedWithSymlink(t *testing.T) {
	installFakePy(t, "3.12.1")
	outside := t.TempDir()
	plantFile(t, filepath.Join(outside, "venv", "precious.txt"), "keep-venv")
	plantFile(t, filepath.Join(outside, "precious.txt"), "keep-root")
	before := snapshotTree(t, outside)

	lexical, real, ancestor := rootUnderLink(t)
	pyenv := filepath.Join(real, "pyenv")
	if err := os.Mkdir(pyenv, 0o755); err != nil {
		t.Fatal(err)
	}
	testAfterPathCheck = func() {
		if err := os.RemoveAll(pyenv); err != nil {
			t.Errorf("撤掉 pyenv: %v", err)
			return
		}
		if err := os.Symlink(outside, pyenv); err != nil {
			t.Errorf("换成符号链接: %v", err)
		}
	}
	t.Cleanup(func() { testAfterPathCheck = nil })

	_, err := Config{Root: lexical, SearchPaths: []string{}}.Ensure(context.Background())
	assertEscapeAt(t, err, pyenv, ancestor)
	if got := snapshotTree(t, outside); got != before {
		t.Fatalf("检查之后换成符号链接，Root 外的文件仍被改动\n之前:\n%s\n之后:\n%s", before, got)
	}
	target, readErr := os.Readlink(pyenv)
	if readErr != nil || target != outside {
		t.Fatalf("pyenv 符号链接 = %q, %v", target, readErr)
	}
}

func TestEnsureRootUnderSymlinkAncestor(t *testing.T) {
	installFakePy(t, "3.12.1")
	lexical, real, ancestor := rootUnderLink(t)
	root := filepath.Join(lexical, "app")
	rt, err := Config{Root: root, SearchPaths: []string{}}.Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	python := filepath.Join(real, "app", "pyenv", "venv", "bin", "python")
	if _, err := os.Stat(python); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rt.Python); err != nil {
		t.Fatalf("返回的解释器不可用: %s: %v", rt.Python, err)
	}
	marker := filepath.Join(real, "app", "pyenv", "venv", ".lantern-requirements.sha256")
	if _, err := os.Stat(marker); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(ancestor)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("祖先符号链接被改掉了: %v %+v", err, info)
	}
}

func TestPathErrorCode(t *testing.T) {
	err := pathEscape("/tmp/app/pyenv", "路径上有符号链接")
	assertPathEscape(t, err)
	if !strings.Contains(err.Error(), "path_escape") || !strings.Contains(err.Error(), "/tmp/app/pyenv") {
		t.Fatalf("err=%v", err)
	}
}

// rootUnderLink 把一个真实目录挂到符号链接下面，模拟 macOS 上 /var、/tmp 这种祖先链接。
// 返回的 real 是 EvalSymlinks 之后的工作区，ancestor 是那个故意加上的链接。
func rootUnderLink(t *testing.T) (lexical, real, ancestor string) {
	t.Helper()
	realParent := t.TempDir()
	workspace := filepath.Join(realParent, "workspace")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	ancestor = filepath.Join(base, "link")
	if err := os.Symlink(realParent, ancestor); err != nil {
		t.Fatal(err)
	}
	lexical = filepath.Join(ancestor, "workspace")
	resolved, err := filepath.EvalSymlinks(lexical)
	if err != nil {
		t.Fatal(err)
	}
	real = filepath.Clean(resolved)
	if real == filepath.Clean(lexical) {
		t.Fatal("Root 没有落在符号链接下面")
	}
	return lexical, real, ancestor
}

func assertEscapeAt(t *testing.T, err error, want, ancestor string) {
	t.Helper()
	assertPathEscape(t, err)
	var pe *PathError
	if !errors.As(err, &pe) || pe == nil {
		t.Fatalf("err=%v，想要 *PathError", err)
	}
	if filepath.Clean(pe.Path) != filepath.Clean(want) {
		t.Fatalf("拒绝的路径 = %s，想要故意制造的链接 %s", pe.Path, want)
	}
	anc := filepath.Clean(ancestor)
	if pe.Path == anc || strings.HasPrefix(pe.Path, anc+string(filepath.Separator)) {
		t.Fatalf("拒绝的是祖先符号链接 %s，不是 %s", pe.Path, want)
	}
}

func assertPathEscape(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrPathEscape) {
		t.Fatalf("err=%v，想要 path_escape", err)
	}
	if errors.Is(err, ErrUnavailable) {
		t.Fatal("路径逃逸不应该是 runtime_unavailable")
	}
	code, ok := errcode.CodeOf(err)
	if !ok || code != errcode.PathEscape || code.String() != "path_escape" {
		t.Fatalf("CodeOf=%s ok=%v", code, ok)
	}
}

func installFakePy(t *testing.T, version string) {
	t.Helper()
	dir := t.TempDir()
	body, err := os.ReadFile("testdata/fake-python.sh")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "python3")
	if err := os.WriteFile(path, body, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".ver", []byte(version+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("PYRT_FAKE_LOG", filepath.Join(t.TempDir(), "log"))
}

func plantFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func snapshotTree(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		b.WriteString(rel)
		b.WriteString(" ")
		b.WriteString(info.Mode().String())
		b.WriteByte('\n')
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			b.WriteString("-> ")
			b.WriteString(target)
			b.WriteByte('\n')
			return nil
		}
		if info.Mode().IsRegular() {
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			b.Write(body)
			b.WriteByte('\n')
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}
