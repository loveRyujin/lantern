//go:build unix

package supervisor

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/sys/unix"
)

func TestFileQAScanDistinguishesAxesAndDoesNotFollowOrKeepUnlinkedBytes(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "sentinel"), bytes.Repeat([]byte{0xA5}, 100<<10), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(outside, "dir"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "dir", "secret"), bytes.Repeat([]byte{0xA5}, 50<<10), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "sentinel"), filepath.Join(root, "file-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "dir"), filepath.Join(root, "dir-link")); err != nil {
		t.Fatal(err)
	}
	linked, err := scanPrivateTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if linked.Followed || linked.Allocated != 0 || linked.LogicalNamed != 0 || linked.Symlinks != 2 || linked.Entries != 2 || linked.Scope != FileScanScope {
		t.Fatalf("symlink targets were attributed to the private tree: %+v", linked)
	}

	payload := filepath.Join(root, "payload")
	if err := os.WriteFile(payload, bytes.Repeat([]byte{0xA5}, 16<<10), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(payload, filepath.Join(root, "payload-link")); err != nil {
		t.Fatal(err)
	}
	hard, err := scanPrivateTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if hard.UniqueInodes != 1 || hard.LogicalNamed != hard.LogicalUnique+(16<<10) || hard.LogicalUnique != 16<<10 || hard.Allocated < 16<<10 || hard.Allocated >= hard.LogicalNamed || hard.Entries != 4 {
		t.Fatalf("hard link was billed twice or missed as a second name: %+v", hard)
	}

	renamed := filepath.Join(root, "moved")
	var before unix.Stat_t
	if err := unix.Lstat(payload, &before); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(payload, renamed); err != nil {
		t.Fatal(err)
	}
	var after unix.Stat_t
	if err := unix.Lstat(renamed, &after); err != nil {
		t.Fatal(err)
	}
	moved, err := scanPrivateTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if before.Ino != after.Ino || moved.UniqueInodes != 1 || moved.Entries != 4 || moved.Allocated != hard.Allocated || moved.LogicalUnique != hard.LogicalUnique {
		t.Fatalf("rename became a second resource: before %d after %d scan %+v", before.Ino, after.Ino, moved)
	}
	if _, err := os.Lstat(payload); !os.IsNotExist(err) {
		t.Fatal("old name still exists after rename")
	}

	sparsePath := filepath.Join(root, "sparse")
	sf, err := os.OpenFile(sparsePath, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := sf.Truncate(1 << 20); err != nil {
		t.Fatal(err)
	}
	if _, err := sf.WriteAt([]byte{0xA5}, (1<<20)-1); err != nil {
		t.Fatal(err)
	}
	// Darwin allocates that span; punch it back before the scan. Other Unix
	// filesystems already leave the hole, and reclaimSparseSpan is a no-op.
	if err := reclaimSparseSpan(int(sf.Fd())); err != nil {
		t.Fatal(err)
	}
	if err := sf.Sync(); err != nil {
		t.Fatal(err)
	}
	sf.Close()
	sparse, err := scanPrivateTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if sparse.LogicalUnique < 1<<20 || sparse.Allocated >= sparse.LogicalUnique/8 || (runtime.GOOS != "darwin" && sparse.StatBlocks != sparse.Allocated) {
		t.Fatalf("sparse logical length was used as allocated space: %+v", sparse)
	}

	heldPath := filepath.Join(root, "held")
	hf, err := os.OpenFile(heldPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hf.Write(bytes.Repeat([]byte{0xA5}, 8<<10)); err != nil {
		t.Fatal(err)
	}
	if err := hf.Sync(); err != nil {
		t.Fatal(err)
	}
	visible, err := scanPrivateTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if visible.Allocated <= sparse.Allocated {
		t.Fatalf("named file was invisible before unlink: before %+v after %+v", sparse, visible)
	}
	if err := os.Remove(heldPath); err != nil {
		t.Fatal(err)
	}
	if _, err := hf.Write(bytes.Repeat([]byte{0xA5}, 32<<10)); err != nil {
		t.Fatal(err)
	}
	if err := hf.Sync(); err != nil {
		t.Fatal(err)
	}
	heldFD := int(hf.Fd())
	var st unix.Stat_t
	if err := unix.Fstat(heldFD, &st); err != nil {
		t.Fatal(err)
	}
	hidden, err := scanPrivateTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if st.Nlink != 0 || st.Size < 40<<10 || hidden.Allocated != sparse.Allocated {
		t.Fatalf("unlinked-open bytes remained visible or the fd died early: nlink %d size %d hidden %+v sparse %+v", st.Nlink, st.Size, hidden, sparse)
	}
	if err := hf.Close(); err != nil {
		t.Fatal(err)
	}
	if err := unix.Fstat(heldFD, &unix.Stat_t{}); err == nil {
		t.Fatal("fd stayed live after close")
	}
	afterClose, err := scanPrivateTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if afterClose.Allocated != hidden.Allocated {
		t.Fatalf("close changed the directory-scan total, which cannot see the reclaimed unlinked bytes: hidden %+v closed %+v", hidden, afterClose)
	}
}
