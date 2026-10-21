package pyrt

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnsureSweepsRenameCrashLeftovers 模拟 installVenv 在两次 rename 中间崩溃：
// 旧 venv 已经改名为 .venv-old-*，新目录 .venv-new-* 还没改名为 venv。
// 下次 Ensure 只删这些真实目录，不跟随同名符号链接。
func TestEnsureSweepsRenameCrashLeftovers(t *testing.T) {
	installFakePy(t, "3.12.1")
	logPath := filepath.Join(t.TempDir(), "log")
	t.Setenv("PYRT_FAKE_LOG", logPath)

	outside := t.TempDir()
	plantFile(t, filepath.Join(outside, "precious.txt"), "keep-outside")
	before := snapshotTree(t, outside)

	lexical, real, ancestor := rootUnderLink(t)
	pyenv := filepath.Join(real, "pyenv")
	if err := os.Mkdir(pyenv, 0o755); err != nil {
		t.Fatal(err)
	}
	oldDir := filepath.Join(pyenv, ".venv-old-111")
	newDir := filepath.Join(pyenv, ".venv-new-222")
	plantFile(t, filepath.Join(oldDir, "from-old.txt"), "old-venv")
	if err := os.Symlink(filepath.Join(outside, "precious.txt"), filepath.Join(oldDir, "escape")); err != nil {
		t.Fatal(err)
	}
	plantFile(t, filepath.Join(newDir, "partial.txt"), "new-partial")
	if err := os.WriteFile(filepath.Join(pyenv, ".venv-new-note.txt"), []byte("keep-file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(pyenv, ".venv-backup"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(pyenv, ".venv-old-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(pyenv, ".venv-new-link")); err != nil {
		t.Fatal(err)
	}

	rt, err := Config{Root: lexical, SearchPaths: []string{}}.Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(oldDir); !os.IsNotExist(err) {
		t.Fatalf(".venv-old 残留还在: %v", err)
	}
	if _, err := os.Lstat(newDir); !os.IsNotExist(err) {
		t.Fatalf(".venv-new 残留还在: %v", err)
	}
	note, err := os.ReadFile(filepath.Join(pyenv, ".venv-new-note.txt"))
	if err != nil || string(note) != "keep-file" {
		t.Fatalf("非目录残留 = %q, %v", note, err)
	}
	if _, err := os.Lstat(filepath.Join(pyenv, ".venv-backup")); err != nil {
		t.Fatalf("前缀对不上的目录不该删: %v", err)
	}
	for _, name := range []string{".venv-old-link", ".venv-new-link"} {
		got, err := os.Readlink(filepath.Join(pyenv, name))
		if err != nil || got != outside {
			t.Fatalf("%s = %q, %v", name, got, err)
		}
	}
	if got := snapshotTree(t, outside); got != before {
		t.Fatalf("符号链接指向的目录被改动了\n之前:\n%s\n之后:\n%s", before, got)
	}
	python := filepath.Join(real, "pyenv", "venv", "bin", "python")
	if rt.Python != python {
		t.Fatalf("Python=%s，想要 %s", rt.Python, python)
	}
	if _, err := os.Stat(python); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(ancestor)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("祖先符号链接被改掉了: %v %+v", err, info)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(log), " -m venv ") != 1 || strings.Contains(string(log), "--clear") {
		t.Fatalf("清掉残留后应该新建一次 venv，且不带 --clear:\n%s", log)
	}
}

// TestEnsureSweepsLeftoversWhenAlreadyReady 覆盖标记仍有效的路径：
// 残留目录要删掉，但不能因此再跑一次 pip。
func TestEnsureSweepsLeftoversWhenAlreadyReady(t *testing.T) {
	installFakePy(t, "3.12.1")
	logPath := filepath.Join(t.TempDir(), "log")
	t.Setenv("PYRT_FAKE_LOG", logPath)

	outside := t.TempDir()
	plantFile(t, filepath.Join(outside, "precious.txt"), "keep-outside")
	before := snapshotTree(t, outside)

	root := t.TempDir()
	cfg := Config{Root: root, SearchPaths: []string{}}
	if _, err := cfg.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	pyenv := filepath.Join(root, "pyenv")
	stale := filepath.Join(pyenv, ".venv-old-stale")
	partial := filepath.Join(pyenv, ".venv-new-stale")
	plantFile(t, filepath.Join(stale, "old.txt"), "stale")
	plantFile(t, filepath.Join(partial, "new.txt"), "partial")
	if err := os.Symlink(outside, filepath.Join(pyenv, ".venv-old-link")); err != nil {
		t.Fatal(err)
	}

	if _, err := cfg.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatalf("已就绪时 .venv-old 残留还在: %v", err)
	}
	if _, err := os.Lstat(partial); !os.IsNotExist(err) {
		t.Fatalf("已就绪时 .venv-new 残留还在: %v", err)
	}
	got, err := os.Readlink(filepath.Join(pyenv, ".venv-old-link"))
	if err != nil || got != outside {
		t.Fatalf("符号链接 = %q, %v", got, err)
	}
	if got := snapshotTree(t, outside); got != before {
		t.Fatalf("已就绪时仍跟着符号链接删了 Root 外的文件\n之前:\n%s\n之后:\n%s", before, got)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(log), " -m pip ") != 1 || strings.Count(string(log), " -m venv ") != 1 {
		t.Fatalf("标记仍有效时不应该重建:\n%s", log)
	}
}
