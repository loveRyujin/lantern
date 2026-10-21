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

	root := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "pyenv")); err != nil {
		t.Fatal(err)
	}
	_, err := Config{Root: root, SearchPaths: []string{}}.Ensure(context.Background())
	assertPathEscape(t, err)
	if got := snapshotTree(t, outside); got != before {
		t.Fatalf("Root 外的文件被改动了\n之前:\n%s\n之后:\n%s", before, got)
	}
}

func TestEnsureRejectsIntermediateSymlink(t *testing.T) {
	installFakePy(t, "3.12.1")
	outside := t.TempDir()
	plantFile(t, filepath.Join(outside, "precious.txt"), "keep")
	before := snapshotTree(t, outside)

	base := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(base, "mid")); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "mid", "app")
	_, err := Config{Root: root, SearchPaths: []string{}}.Ensure(context.Background())
	assertPathEscape(t, err)
	if got := snapshotTree(t, outside); got != before {
		t.Fatalf("中间层符号链接指向的目录被改动了\n之前:\n%s\n之后:\n%s", before, got)
	}
	if _, statErr := os.Lstat(filepath.Join(outside, "app")); !os.IsNotExist(statErr) {
		t.Fatalf("顺着中间层符号链接创建了目录: %v", statErr)
	}
}

func TestEnsureTOCTOUPyenvReplacedWithSymlink(t *testing.T) {
	installFakePy(t, "3.12.1")
	outside := t.TempDir()
	plantFile(t, filepath.Join(outside, "venv", "precious.txt"), "keep-venv")
	plantFile(t, filepath.Join(outside, "precious.txt"), "keep-root")
	before := snapshotTree(t, outside)

	root := t.TempDir()
	pyenv := filepath.Join(root, "pyenv")
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

	_, err := Config{Root: root, SearchPaths: []string{}}.Ensure(context.Background())
	assertPathEscape(t, err)
	if got := snapshotTree(t, outside); got != before {
		t.Fatalf("检查之后换成符号链接，Root 外的文件仍被改动\n之前:\n%s\n之后:\n%s", before, got)
	}
	target, readErr := os.Readlink(pyenv)
	if readErr != nil || target != outside {
		t.Fatalf("pyenv 符号链接 = %q, %v", target, readErr)
	}
}

func TestPathErrorCode(t *testing.T) {
	err := pathEscape("/tmp/app/pyenv", "路径上有符号链接")
	assertPathEscape(t, err)
	if !strings.Contains(err.Error(), "path_escape") || !strings.Contains(err.Error(), "/tmp/app/pyenv") {
		t.Fatalf("err=%v", err)
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
