//go:build linux && amd64

package sandbox

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var errTaskFDNotReady = errors.New("task FD inspection temporarily unavailable")

func sampleTask(pid int) (rss, hwm uint64, cpu time.Duration, err error) {
	file, e := os.Open(fmt.Sprintf("/proc/%d/status", pid))
	if e != nil {
		err = e
		return
	}
	s := bufio.NewScanner(file)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) < 2 {
			continue
		}
		if fields[0] == "VmRSS:" || fields[0] == "VmHWM:" {
			n, e := strconv.ParseUint(fields[1], 10, 64)
			if e != nil {
				continue
			}
			if fields[0] == "VmRSS:" {
				rss = n * 1024
			} else {
				hwm = n * 1024
			}
		}
	}
	err = s.Err()
	file.Close()
	if err != nil {
		return
	}
	data, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if e != nil {
		err = e
		return
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		err = fmt.Errorf("invalid proc stat")
		return
	}
	fields := strings.Fields(string(data)[end+1:])
	if len(fields) < 13 {
		err = fmt.Errorf("short proc stat")
		return
	}
	u, e := strconv.ParseUint(fields[11], 10, 64)
	if e != nil {
		err = e
		return
	}
	system, e := strconv.ParseUint(fields[12], 10, 64)
	if e != nil {
		err = e
		return
	}
	// Linux proc reports USER_HZ=100 on supported amd64, independent of kernel HZ.
	cpu = time.Duration(u+system) * 10 * time.Millisecond
	return
}
func checkWorkspace(root string, pid int) error {
	var total int64
	count := 0
	seen := map[[2]uint64]bool{}
	addFile := func(info os.FileInfo) error {
		if !info.Mode().IsRegular() {
			return nil
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return Failed("cannot inspect workspace inode")
		}
		key := [2]uint64{uint64(stat.Dev), stat.Ino}
		if seen[key] {
			return nil
		}
		seen[key] = true
		size := info.Size()
		if allocated := stat.Blocks * 512; allocated > size {
			size = allocated
		}
		total += size
		if total > maxWorkBytes {
			return Failed("workspace exceeds sampled 64 MiB budget")
		}
		return nil
	}
	for _, dir := range []string{"out", "tmp"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			count++
			if count > maxWorkEntries {
				return Failed("workspace exceeds 1024 entries")
			}
			info, e := d.Info()
			if os.IsNotExist(e) {
				return nil
			}
			if e != nil {
				return e
			}
			return addFile(info)
		})
		if err != nil {
			return err
		}
	}
	// Unlink does not release disk while a descriptor remains open. Include those
	// live inodes once, even if a task hides them from the directory traversal.
	if pid <= 0 {
		return nil
	} // The single task was reaped; no deleted-open inodes remain.
	fds, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		if os.IsPermission(err) {
			return taskFDPermission(pid, err)
		}
		return err
	}
	for _, fd := range fds {
		path := fmt.Sprintf("/proc/%d/fd/%s", pid, fd.Name())
		target, e := os.Readlink(path)
		if e != nil {
			continue
		}
		if !strings.HasSuffix(target, " (deleted)") {
			continue
		}
		if !strings.HasPrefix(target, filepath.Join(root, "out")+"/") && !strings.HasPrefix(target, filepath.Join(root, "tmp")+"/") {
			continue
		}
		info, e := os.Stat(path)
		if os.IsNotExist(e) {
			continue
		}
		// Exit can make one descriptor EACCES after the directory listing
		// succeeded. That is the same teardown race as a directory-level
		// permission error, not a live task hiding deleted files.
		if os.IsPermission(e) {
			return taskFDPermission(pid, e)
		}
		if e != nil {
			return e
		}
		if e = addFile(info); e != nil {
			return e
		}
	}
	return nil
}

// taskFDPermission reports a gone or zombie task as success. A still-live task
// whose descriptors cannot be read stays retryable for a short window.
func taskFDPermission(pid int, err error) error {
	stat, readErr := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if readErr != nil && os.IsNotExist(readErr) {
		return nil
	}
	if end := strings.LastIndexByte(string(stat), ')'); end >= 0 {
		fields := strings.Fields(string(stat[end+1:]))
		if len(fields) > 0 && (fields[0] == "Z" || fields[0] == "X") {
			return nil
		}
	}
	return fmt.Errorf("%w: %v", errTaskFDNotReady, err)
}
