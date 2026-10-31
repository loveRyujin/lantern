//go:build unix

package supervisor

import (
	"fmt"
	"math"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// scanPrivateTree meters one exclusive directory without following symlinks.
// Allocated bytes count each regular inode once. LogicalNamed sums every
// regular name, so a hard link increases it without increasing Allocated.
func scanPrivateTree(root string) (FileScan, error) {
	out := FileScan{Scope: FileScanScope}
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return FileScan{}, fmt.Errorf("file scan root is not an absolute private directory")
	}
	seen := map[[2]uint64]struct{}{}
	err := filepath.WalkDir(root, func(path string, _ os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if path == root {
			return nil
		}
		var st unix.Stat_t
		if e := unix.Lstat(path, &st); e != nil {
			if os.IsNotExist(e) {
				return nil
			}
			return e
		}
		out.Entries++
		if st.Mode&unix.S_IFMT == unix.S_IFLNK {
			out.Symlinks++
			return nil
		}
		if st.Mode&unix.S_IFMT != unix.S_IFREG {
			return nil
		}
		if st.Size < 0 || st.Blocks < 0 {
			return fmt.Errorf("negative file stat at %s", filepath.Base(path))
		}
		size := uint64(st.Size)
		blocks := uint64(st.Blocks)
		if blocks > math.MaxUint64/512 || math.MaxUint64-out.LogicalNamed < size {
			return fmt.Errorf("file stat sum overflow at %s", filepath.Base(path))
		}
		out.LogicalNamed += size
		key := [2]uint64{uint64(st.Dev), uint64(st.Ino)}
		if _, ok := seen[key]; ok {
			return nil
		}
		seen[key] = struct{}{}
		alloc, e := allocatedRegularBytes(path, st)
		if e != nil {
			return e
		}
		physical := blocks * 512
		if math.MaxUint64-out.StatBlocks < physical || math.MaxUint64-out.Allocated < alloc || math.MaxUint64-out.LogicalUnique < size {
			return fmt.Errorf("file stat sum overflow at %s", filepath.Base(path))
		}
		out.UniqueInodes++
		out.LogicalUnique += size
		out.StatBlocks += physical
		out.Allocated += alloc
		return nil
	})
	if err != nil {
		return FileScan{}, err
	}
	return out, nil
}
