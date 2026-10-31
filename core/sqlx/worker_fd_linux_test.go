//go:build linux

package sqlx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The parent keeps a descriptor with FD_CLOEXEC cleared and points it at a
// temporary file. NewTool starts a real worker. After the second exec, that
// file must not show up in the worker's /proc/<pid>/fd. Removing
// closeInheritedDescriptors (or skipping it before Exec) makes this fail.
// A compile error, a worker that never reaches --lantern-sql-worker-v1, or a
// parent that already closed the sentinel is not a successful mutation.
func TestSQLWorkerDoesNotInheritClearedCloexecFD(t *testing.T) {
	sensitive := filepath.Join(t.TempDir(), "sensitive-token")
	if err := os.WriteFile(sensitive, []byte("secret-sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	sentinel, err := os.Open(sensitive)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sentinel.Close() })
	fd := int(sentinel.Fd())
	if fd < 3 {
		t.Fatalf("sentinel fd %d is stdio; this test only covers descriptors ≥ 3", fd)
	}
	if err := setFDFlags(fd, 0); err != nil {
		t.Fatal(err)
	}
	flags, err := getFDFlags(fd)
	if err != nil {
		t.Fatal(err)
	}
	if flags&syscall.FD_CLOEXEC != 0 {
		t.Fatal("sentinel still has FD_CLOEXEC; Exec would close it and the test would not see a leak")
	}
	parentTarget, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
	if err != nil || parentTarget == "" {
		t.Fatalf("parent sentinel target: %q %v", parentTarget, err)
	}

	path := processTestPath(t)
	tool, err := NewTool(parentMustNotQuery{t}, path, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	type outcome struct {
		err error
	}
	// No table access. The recursive sum keeps the worker alive long enough
	// to read /proc/<pid>/fd after the second exec. Build the call here so
	// the worker goroutine does not touch the testing.T.
	query := "WITH RECURSIVE x(n) AS (VALUES(0) UNION ALL SELECT n+1 FROM x WHERE n<20000000) SELECT sum(n) FROM x"
	call := sqlCall(t, query)
	done := make(chan outcome, 1)
	go func() {
		_, err := tool.Execute(ctx, call)
		done <- outcome{err}
	}()

	var pid int
	var targets map[string]string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case out := <-done:
			t.Fatalf("worker finished before the fd probe: %v", out.err)
		default:
		}
		pids := sqlWorkerChildPIDs()
		if len(pids) != 1 {
			time.Sleep(time.Millisecond)
			continue
		}
		got, err := workerFDTargets(pids[0])
		if err != nil {
			time.Sleep(time.Millisecond)
			continue
		}
		pid, targets = pids[0], got
		break
	}
	if pid == 0 {
		t.Fatal("timed out waiting for a running --lantern-sql-worker-v1 child")
	}
	again, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
	if err != nil || again != parentTarget {
		t.Fatalf("parent dropped the sentinel before the probe: %q %v", again, err)
	}
	for name, target := range targets {
		if target == parentTarget || strings.HasPrefix(target, parentTarget+" ") {
			t.Fatalf("worker %d fd %s inherited sentinel %s; table=%v", pid, name, parentTarget, targets)
		}
	}
	t.Logf("worker %d fds=%v; sentinel %d stayed in the parent only", pid, targets, fd)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not finish after cancellation")
	}
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); !os.IsNotExist(err) {
		t.Fatalf("worker %d was not reaped: %v", pid, err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("canceled worker still exists: %v", err)
	}
}

func setFDFlags(fd int, flags uintptr) error {
	_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_SETFD, flags)
	if errno != 0 {
		return errno
	}
	return nil
}

func getFDFlags(fd int) (uintptr, error) {
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0)
	if errno != 0 {
		return 0, errno
	}
	return flags, nil
}

func sqlWorkerChildPIDs() []int {
	matches, _ := filepath.Glob("/proc/self/task/*/children")
	seen := make(map[int]bool)
	var pids []int
	for _, file := range matches {
		body, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		for _, field := range strings.Fields(string(body)) {
			pid, err := strconv.Atoi(field)
			if err != nil || seen[pid] {
				continue
			}
			seen[pid] = true
			args, err := os.ReadFile("/proc/" + field + "/cmdline")
			if err != nil || !strings.Contains(string(args), workerArg) {
				continue
			}
			pids = append(pids, pid)
		}
	}
	return pids
}

func workerFDTargets(pid int) (map[string]string, error) {
	dir := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	targets := make(map[string]string, len(entries))
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		targets[entry.Name()] = target
	}
	for _, name := range []string{"0", "1", "2"} {
		if _, ok := targets[name]; !ok {
			return nil, fmt.Errorf("worker %d missing stdio fd %s", pid, name)
		}
	}
	return targets, nil
}
