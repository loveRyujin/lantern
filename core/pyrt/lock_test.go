package pyrt

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/loveRyujin/lantern/core/errcode"
)

func TestEnsureBusyDoesNotSweep(t *testing.T) {
	installFakePy(t, "3.12.1")
	outside := t.TempDir()
	plantFile(t, filepath.Join(outside, "precious.txt"), "keep-outside")
	before := snapshotTree(t, outside)

	root := t.TempDir()
	pyenv := filepath.Join(root, "pyenv")
	if err := os.Mkdir(pyenv, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(pyenv, ".venv-old-stale", "old.txt")
	plantFile(t, stale, "stale")
	if err := os.Symlink(outside, filepath.Join(pyenv, ".venv-new-link")); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockEnsure(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlock)

	_, err = Config{Root: root, SearchPaths: []string{}}.Ensure(context.Background())
	if !errors.Is(err, errEnsureBusy) {
		t.Fatalf("err=%v，想要已有一次 Ensure 在进行", err)
	}
	if errors.Is(err, ErrUnavailable) || errors.Is(err, ErrPathEscape) {
		t.Fatal("锁冲突不应该是 runtime_unavailable 或 path_escape")
	}
	if code, ok := errcode.CodeOf(err); ok {
		t.Fatalf("锁冲突不应该带错误码 %s", code)
	}
	body, err := os.ReadFile(stale)
	if err != nil || string(body) != "stale" {
		t.Fatalf("拿不到锁时清掉了残留: %q %v", body, err)
	}
	got, err := os.Readlink(filepath.Join(pyenv, ".venv-new-link"))
	if err != nil || got != outside {
		t.Fatalf("符号链接 = %q, %v", got, err)
	}
	if snap := snapshotTree(t, outside); snap != before {
		t.Fatalf("拿不到锁时改动了 Root 外的文件\n之前:\n%s\n之后:\n%s", before, snap)
	}
	if _, err := os.Stat(filepath.Join(pyenv, "venv")); !os.IsNotExist(err) {
		t.Fatal("拿不到锁时不应该创建 venv")
	}
}
