package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoveMissingDoesNothing(t *testing.T) {
	root := t.TempDir()
	if err := Replaceable(root, "星禾"); err != nil {
		t.Fatal(err)
	}
	if err := Remove(root, "星禾"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "workspaces")); !os.IsNotExist(err) {
		t.Fatalf("Remove created workspaces: %v", err)
	}
}

func TestRemoveDeletesOnlyThatWorkspace(t *testing.T) {
	root := t.TempDir()
	keep := filepath.Join(root, "workspaces", "云杉", "keep.txt")
	writeTiny(t, keep, "keep")
	dir := filepath.Join(root, "workspaces", "星禾")
	writeTiny(t, filepath.Join(dir, "gone.txt"), "gone")
	outside := filepath.Join(t.TempDir(), "secret.txt")
	writeTiny(t, outside, "secret")
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := Remove(root, "星禾"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("workspace still present: %v", err)
	}
	if body, err := os.ReadFile(keep); err != nil || string(body) != "keep" {
		t.Fatalf("sibling: %v %q", err, body)
	}
	if body, err := os.ReadFile(outside); err != nil || string(body) != "secret" {
		t.Fatalf("symlink target: %v %q", err, body)
	}
}

func TestRemoveRejectsSymlinks(t *testing.T) {
	t.Run("workspaces", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		secret := filepath.Join(outside, "星禾", "keep.txt")
		writeTiny(t, secret, "keep")
		if err := os.Symlink(outside, filepath.Join(root, "workspaces")); err != nil {
			t.Fatal(err)
		}
		err := Remove(root, "星禾")
		if !errors.Is(err, ErrPathEscape) {
			t.Fatalf("Remove: %v", err)
		}
		if body, err := os.ReadFile(secret); err != nil || string(body) != "keep" {
			t.Fatalf("target: %v %q", err, body)
		}
	})

	t.Run("workspace", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		secret := filepath.Join(outside, "keep.txt")
		writeTiny(t, secret, "keep")
		link := filepath.Join(root, "workspaces", "星禾")
		if err := os.Mkdir(filepath.Dir(link), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
		err := Remove(root, "星禾")
		if !errors.Is(err, ErrPathEscape) {
			t.Fatalf("Remove: %v", err)
		}
		info, err := os.Lstat(link)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Fatal("symlink was replaced")
		}
		if body, err := os.ReadFile(secret); err != nil || string(body) != "keep" {
			t.Fatalf("target: %v %q", err, body)
		}
	})
}

func TestRemoveRejectsFileAndEscape(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "workspaces", "星禾")
	writeTiny(t, file, "not-a-dir")
	if err := Remove(root, "星禾"); !errors.Is(err, ErrPathEscape) {
		t.Fatalf("file: %v", err)
	}
	if body, err := os.ReadFile(file); err != nil || string(body) != "not-a-dir" {
		t.Fatalf("file changed: %v %q", err, body)
	}
	if err := Remove(root, "../outside"); !errors.Is(err, ErrPathEscape) {
		t.Fatalf("escape: %v", err)
	}
	if err := Remove(root, ""); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("empty: %v", err)
	}
}

func TestCleanScratch(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, ".lantern-seed-stale", "secret.txt")
	writeTiny(t, stale, "phone")
	outside := filepath.Join(t.TempDir(), "keep.txt")
	writeTiny(t, outside, "keep")
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(root, ".lantern-seed-link")); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(root, "workspaces", "星禾", "keep.txt")
	writeTiny(t, keep, "keep")
	notes := filepath.Join(root, "notes.txt")
	writeTiny(t, notes, "notes")

	if err := CleanScratch(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(stale)); !os.IsNotExist(err) {
		t.Fatalf("stale scratch still present: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, ".lantern-seed-link")); !os.IsNotExist(err) {
		t.Fatalf("scratch symlink still present: %v", err)
	}
	if body, err := os.ReadFile(outside); err != nil || string(body) != "keep" {
		t.Fatalf("symlink target: %v %q", err, body)
	}
	if body, err := os.ReadFile(keep); err != nil || string(body) != "keep" {
		t.Fatalf("workspace: %v %q", err, body)
	}
	if body, err := os.ReadFile(notes); err != nil || string(body) != "notes" {
		t.Fatalf("notes: %v %q", err, body)
	}
	if err := CleanScratch(filepath.Join(root, "missing")); err != nil {
		t.Fatal(err)
	}
}

func TestInstallReplacesBoth(t *testing.T) {
	dst := t.TempDir()
	src := t.TempDir()
	writeTiny(t, filepath.Join(dst, "workspaces", "星禾", "marker.txt"), "old-xh")
	writeTiny(t, filepath.Join(dst, "workspaces", "云杉", "marker.txt"), "old-ys")
	writeTiny(t, filepath.Join(src, "workspaces", "星禾", "marker.txt"), "new-xh")
	writeTiny(t, filepath.Join(src, "workspaces", "云杉", "marker.txt"), "new-ys")

	dirs, err := Install(dst, src, []string{"星禾", "云杉"})
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 2 || filepath.Base(dirs[0]) != "星禾" || filepath.Base(dirs[1]) != "云杉" {
		t.Fatalf("dirs %v", dirs)
	}
	assertFile(t, filepath.Join(dst, "workspaces", "星禾", "marker.txt"), "new-xh")
	assertFile(t, filepath.Join(dst, "workspaces", "云杉", "marker.txt"), "new-ys")
	if _, err := os.Stat(filepath.Join(src, "workspaces", "星禾")); !os.IsNotExist(err) {
		t.Fatalf("source still present: %v", err)
	}
	assertNoScratchDir(t, dst)
}

func TestInstallLeavesDestWhenSourceMissing(t *testing.T) {
	dst := t.TempDir()
	src := t.TempDir()
	writeTiny(t, filepath.Join(dst, "workspaces", "星禾", "marker.txt"), "old-xh")
	writeTiny(t, filepath.Join(dst, "workspaces", "云杉", "marker.txt"), "old-ys")
	writeTiny(t, filepath.Join(src, "workspaces", "星禾", "marker.txt"), "new-xh")

	_, err := Install(dst, src, []string{"星禾", "云杉"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Install: %v", err)
	}
	assertFile(t, filepath.Join(dst, "workspaces", "星禾", "marker.txt"), "old-xh")
	assertFile(t, filepath.Join(dst, "workspaces", "云杉", "marker.txt"), "old-ys")
	assertFile(t, filepath.Join(src, "workspaces", "星禾", "marker.txt"), "new-xh")
	assertNoScratchDir(t, dst)
}

func TestInstallRejectsSymlinkBeforeReplace(t *testing.T) {
	dst := t.TempDir()
	src := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "keep.txt")
	writeTiny(t, secret, "keep")
	if err := os.Mkdir(filepath.Join(dst, "workspaces"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dst, "workspaces", "云杉")); err != nil {
		t.Fatal(err)
	}
	writeTiny(t, filepath.Join(dst, "workspaces", "星禾", "marker.txt"), "old-xh")
	writeTiny(t, filepath.Join(src, "workspaces", "星禾", "marker.txt"), "new-xh")
	writeTiny(t, filepath.Join(src, "workspaces", "云杉", "marker.txt"), "new-ys")

	_, err := Install(dst, src, []string{"星禾", "云杉"})
	if !errors.Is(err, ErrPathEscape) {
		t.Fatalf("Install: %v", err)
	}
	assertFile(t, filepath.Join(dst, "workspaces", "星禾", "marker.txt"), "old-xh")
	assertFile(t, secret, "keep")
	info, err := os.Lstat(filepath.Join(dst, "workspaces", "云杉"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink was replaced")
	}
	assertFile(t, filepath.Join(src, "workspaces", "星禾", "marker.txt"), "new-xh")
}

func TestInstallRollsBackWhenSecondMoveFails(t *testing.T) {
	dst := t.TempDir()
	src := t.TempDir()
	writeTiny(t, filepath.Join(dst, "workspaces", "星禾", "marker.txt"), "old-xh")
	writeTiny(t, filepath.Join(dst, "workspaces", "云杉", "marker.txt"), "old-ys")
	writeTiny(t, filepath.Join(src, "workspaces", "星禾", "marker.txt"), "new-xh")
	writeTiny(t, filepath.Join(src, "workspaces", "云杉", "marker.txt"), "new-ys")

	orig := renamePath
	renamePath = func(oldpath, newpath string) error {
		sep := string(filepath.Separator)
		if strings.HasSuffix(oldpath, sep+"云杉") && strings.Contains(newpath, ".lantern-seed-") {
			return errors.New("disk")
		}
		return orig(oldpath, newpath)
	}
	t.Cleanup(func() { renamePath = orig })

	_, err := Install(dst, src, []string{"星禾", "云杉"})
	if err == nil || !strings.Contains(err.Error(), "disk") {
		t.Fatalf("Install: %v", err)
	}
	assertFile(t, filepath.Join(dst, "workspaces", "星禾", "marker.txt"), "old-xh")
	assertFile(t, filepath.Join(dst, "workspaces", "云杉", "marker.txt"), "old-ys")
	assertFile(t, filepath.Join(src, "workspaces", "星禾", "marker.txt"), "new-xh")
	assertFile(t, filepath.Join(src, "workspaces", "云杉", "marker.txt"), "new-ys")
	assertNoScratchDir(t, dst)
}

func TestInstallFreshRoot(t *testing.T) {
	dst := t.TempDir()
	src := t.TempDir()
	writeTiny(t, filepath.Join(src, "workspaces", "星禾", "marker.txt"), "new-xh")
	dirs, err := Install(dst, src, []string{"星禾"})
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 1 {
		t.Fatalf("dirs %v", dirs)
	}
	assertFile(t, filepath.Join(dst, "workspaces", "星禾", "marker.txt"), "new-xh")
	assertNoScratchDir(t, dst)
}

func writeTiny(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != want {
		t.Fatalf("%s = %q, want %q", path, body, want)
	}
}

func assertNoScratchDir(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".lantern-seed-") {
			t.Fatalf("scratch left behind: %s", entry.Name())
		}
	}
}
