package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestListMissingAndBlank(t *testing.T) {
	got, err := List(filepath.Join(t.TempDir(), "missing"))
	if err != nil || len(got) != 0 {
		t.Fatalf("missing root: %v %v", got, err)
	}
	if _, err := List("  "); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("blank root: %v", err)
	}
}

func TestListReturnsCreatedWorkspaces(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	for _, id := range []string{"星禾", "云杉"} {
		h := mustCreate(t, ctx, root, id)
		t.Cleanup(func() { _ = h.Close() })
	}
	got, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "云杉" || got[1].ID != "星禾" {
		t.Fatalf("List = %+v，想要按标识排序的云杉、星禾", got)
	}
}

func TestListSkipsSymlinkIncompleteAndInvalid(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	h := mustCreate(t, ctx, root, "星禾")
	t.Cleanup(func() { _ = h.Close() })

	base := filepath.Join(root, "workspaces")
	if err := os.Mkdir(filepath.Join(base, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(base, "a b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(base, "linked")); err != nil {
		t.Fatal(err)
	}
	got, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "星禾" {
		t.Fatalf("List = %+v，只应留下星禾", got)
	}
}

func TestListRejectsSymlinkedWorkspacesDir(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "workspaces")); err != nil {
		t.Fatal(err)
	}
	if _, err := List(root); !errors.Is(err, ErrPathEscape) {
		t.Fatalf("symlink workspaces: %v", err)
	}
}
