//go:build darwin

package supervisor

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const fileCreateFlags = unix.O_CREAT | unix.O_EXCL | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_WRONLY

func runFileWorkload(mode Mode, work string) int {
	if !filepath.IsAbs(work) || filepath.Clean(work) != work {
		return 125
	}
	root, e := unix.Open(work, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return 125
	}
	defer unix.Close(root)
	if unix.Mkdirat(root, "out", 0700) != nil || unix.Mkdirat(root, "tmp", 0700) != nil {
		return 125
	}
	out, e := unix.Openat(root, "out", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return 125
	}
	defer unix.Close(out)
	emit := func(cp FileCheckpoint) int {
		if e := (&cp).fillScan(work); e != nil {
			return 125
		}
		r := workerMetadata(mode)
		r.File = &cp
		if json.NewEncoder(os.Stdout).Encode(r) != nil {
			return 125
		}
		return 0
	}
	if emit(FileCheckpoint{Phase: "entered"}) != 0 {
		return 125
	}
	switch mode {
	case FileSmall:
		if writeNamed(out, "a", FileSmallEach) != nil || writeNamed(out, "b", FileSmallEach) != nil {
			return 125
		}
		if emit(FileCheckpoint{Phase: "steady", Written: FileSmallCount * FileSmallEach}) != 0 {
			return 125
		}
		time.Sleep(150 * time.Millisecond)
	case FileMulti:
		for i := 0; i < FileMultiCount; i++ {
			if writeNamed(out, fileComponent(i), FileMultiEach) != nil {
				return 125
			}
			time.Sleep(40 * time.Millisecond)
		}
		if emit(FileCheckpoint{Phase: "steady", Written: FileMultiCount * FileMultiEach}) != 0 {
			return 125
		}
		time.Sleep(150 * time.Millisecond)
	case FileEntries:
		// Six names plus out/ and tmp/ sit on the ceiling (not over it) long
		// enough for a running sample. The rest cross the separate name bound.
		for i := 0; i < FileEntryCount; i++ {
			if createEmpty(out, fileComponent(i)) != nil {
				return 125
			}
			if i == 5 {
				time.Sleep(50 * time.Millisecond)
			}
		}
		if emit(FileCheckpoint{Phase: "steady"}) != 0 {
			return 125
		}
		time.Sleep(200 * time.Millisecond)
	case FileGrow:
		fd, e := unix.Openat(out, "grow", fileCreateFlags, 0600)
		if e != nil {
			return 125
		}
		written := 0
		for written < FileGrowCap {
			if writeDense(fd, FileDenseChunk) != nil {
				unix.Close(fd)
				return 125
			}
			written += FileDenseChunk
			time.Sleep(40 * time.Millisecond)
		}
		unix.Close(fd)
		if emit(FileCheckpoint{Phase: "steady", Written: uint64(written)}) != 0 {
			return 125
		}
		time.Sleep(150 * time.Millisecond)
	case FileHold:
		if writeNamed(out, "hold", FileSmallEach) != nil {
			return 125
		}
		if emit(FileCheckpoint{Phase: "steady", Written: FileSmallEach}) != 0 {
			return 125
		}
		time.Sleep(4 * time.Second)
	case FileEdges:
		cp, e := buildEdgeFixture(out)
		if e != nil || emit(cp) != 0 {
			return 125
		}
		time.Sleep(150 * time.Millisecond)
	case FileUnlink:
		if runUnlinkOpen(out, work, emit) != 0 {
			return 125
		}
	default:
		return 125
	}
	return 0
}

func fileComponent(i int) string {
	return "f" + string(rune('a'+i))
}

// punchUnwrittenSpan deallocates a 4KiB-aligned range and leaves the logical
// size unchanged. APFS otherwise keeps the span physically allocated.
func punchUnwrittenSpan(fd int, offset, length int64) error {
	if offset <= 0 || length <= 0 || offset%4096 != 0 || length%4096 != 0 {
		return unix.EINVAL
	}
	hole := struct {
		flags    uint32
		reserved uint32
		offset   int64
		length   int64
	}{offset: offset, length: length}
	_, _, errno := unix.Syscall(unix.SYS_FCNTL, uintptr(fd), uintptr(unix.F_PUNCHHOLE), uintptr(unsafe.Pointer(&hole)))
	if errno != 0 {
		return errno
	}
	return nil
}

func writeNamed(dir int, name string, n int) error {
	fd, e := unix.Openat(dir, name, fileCreateFlags, 0600)
	if e != nil {
		return e
	}
	defer unix.Close(fd)
	return writeDense(fd, n)
}

func createEmpty(dir int, name string) error {
	fd, e := unix.Openat(dir, name, fileCreateFlags, 0600)
	if e != nil {
		return e
	}
	return unix.Close(fd)
}

func writeDense(fd, n int) error {
	buf := make([]byte, 8192)
	for i := range buf {
		buf[i] = 0xA5
	}
	left := n
	for left > 0 {
		chunk := buf
		if left < len(chunk) {
			chunk = buf[:left]
		}
		wrote, e := unix.Write(fd, chunk)
		if wrote > 0 {
			left -= wrote
		}
		if e != nil {
			return e
		}
		if wrote == 0 {
			return unix.EIO
		}
	}
	return unix.Fsync(fd)
}

func (cp *FileCheckpoint) fillScan(root string) error {
	s, e := scanPrivateTree(root)
	if e != nil {
		return e
	}
	cp.LogicalNamed = s.LogicalNamed
	cp.LogicalUnique = s.LogicalUnique
	cp.Allocated = s.Allocated
	cp.StatBlocks = s.StatBlocks
	cp.Entries = s.Entries
	cp.UniqueInodes = s.UniqueInodes
	cp.Symlinks = s.Symlinks
	return nil
}

func noteLive(cp *FileCheckpoint, fd int) error {
	var st unix.Stat_t
	if e := unix.Fstat(fd, &st); e != nil {
		return e
	}
	cp.Live = true
	cp.Dev = uint64(st.Dev)
	cp.Ino = uint64(st.Ino)
	cp.NLink = uint64(st.Nlink)
	return nil
}

func buildEdgeFixture(out int) (FileCheckpoint, error) {
	cp := FileCheckpoint{Phase: "steady", Written: FileEdgeEach*2 + 1}
	fd, e := unix.Openat(out, "sparse", fileCreateFlags, 0600)
	if e != nil {
		return cp, e
	}
	// One byte at the start, then extend. APFS allocates the whole span of a
	// tail write into a large file (measured on macOS 15.8.2). Punch that
	// unwritten span before any later sample can bill it as payload.
	if _, e = unix.Pwrite(fd, []byte{0xA5}, 0); e != nil {
		unix.Close(fd)
		return cp, e
	}
	if e = unix.Ftruncate(fd, FileSparseLogical); e != nil {
		unix.Close(fd)
		return cp, e
	}
	if e = punchUnwrittenSpan(fd, 4096, FileSparseLogical-4096); e != nil {
		unix.Close(fd)
		return cp, e
	}
	e = unix.Fsync(fd)
	unix.Close(fd)
	if e != nil {
		return cp, e
	}
	if e = writeNamed(out, "data", FileEdgeEach); e != nil {
		return cp, e
	}
	if e = unix.Linkat(out, "data", out, "data-link", 0); e != nil {
		return cp, e
	}
	var a, b unix.Stat_t
	if e = unix.Fstatat(out, "data", &a, unix.AT_SYMLINK_NOFOLLOW); e != nil {
		return cp, e
	}
	if e = unix.Fstatat(out, "data-link", &b, unix.AT_SYMLINK_NOFOLLOW); e != nil {
		return cp, e
	}
	if a.Ino == 0 || a.Ino != b.Ino || a.Dev != b.Dev {
		return cp, unix.EIO
	}
	cp.SameInodeNames = 2
	if e = writeNamed(out, "old", FileEdgeEach); e != nil {
		return cp, e
	}
	var before unix.Stat_t
	if e = unix.Fstatat(out, "old", &before, unix.AT_SYMLINK_NOFOLLOW); e != nil {
		return cp, e
	}
	if e = unix.Renameat(out, "old", out, "renamed"); e != nil {
		return cp, e
	}
	var after unix.Stat_t
	if e = unix.Fstatat(out, "renamed", &after, unix.AT_SYMLINK_NOFOLLOW); e != nil {
		return cp, e
	}
	if e = unix.Fstatat(out, "old", &unix.Stat_t{}, unix.AT_SYMLINK_NOFOLLOW); !errors.Is(e, unix.ENOENT) {
		return cp, unix.EIO
	}
	cp.RenameSameInode = before.Ino != 0 && before.Ino == after.Ino && before.Dev == after.Dev
	if !cp.RenameSameInode {
		return cp, unix.EIO
	}
	if e = unix.Symlinkat("data", out, "alias"); e != nil {
		return cp, e
	}
	// up points at the parent of the work root. The scanner must not walk it.
	if e = unix.Symlinkat("../..", out, "up"); e != nil {
		return cp, e
	}
	return cp, nil
}

func runUnlinkOpen(out int, work string, emit func(FileCheckpoint) int) int {
	fd, e := unix.Openat(out, "held", fileCreateFlags, 0600)
	if e != nil {
		return 125
	}
	defer func() {
		if fd >= 0 {
			unix.Close(fd)
		}
	}()
	if writeDense(fd, FileUnlinkNamed) != nil {
		return 125
	}
	named := FileCheckpoint{Phase: "named", Written: FileUnlinkNamed}
	if noteLive(&named, fd) != nil || named.NLink < 1 || emit(named) != 0 {
		return 125
	}
	time.Sleep(100 * time.Millisecond)
	if unix.Unlinkat(out, "held", 0) != nil {
		return 125
	}
	unlinked := FileCheckpoint{Phase: "unlinked", Written: FileUnlinkNamed, Unlinked: true}
	if noteLive(&unlinked, fd) != nil || unlinked.NLink != 0 || emit(unlinked) != 0 {
		return 125
	}
	if writeDense(fd, FileUnlinkExtra) != nil {
		return 125
	}
	grew := FileCheckpoint{Phase: "grew", Written: FileUnlinkNamed + FileUnlinkExtra, Unlinked: true}
	if noteLive(&grew, fd) != nil || grew.NLink != 0 || grew.Dev != named.Dev || grew.Ino != named.Ino || emit(grew) != 0 {
		return 125
	}
	time.Sleep(200 * time.Millisecond)
	if unix.Close(fd) != nil {
		return 125
	}
	fd = -1
	closed := FileCheckpoint{Phase: "closed", Written: grew.Written, Dev: grew.Dev, Ino: grew.Ino, Unlinked: true, Closed: true}
	if emit(closed) != 0 {
		return 125
	}
	time.Sleep(50 * time.Millisecond)
	return 0
}
