//go:build linux && amd64

package sandbox

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A non-dumpable task keeps running while the kernel makes its proc fd
// directory root-owned: the live counterpart of the exit_mm window.
func TestSandboxFDHelperNonDumpable(t *testing.T) {
	if os.Getenv("LANTERN_FD_HELPER_NONDUMPABLE") != "1" {
		return
	}
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		os.Exit(2)
	}
	os.Stdout.WriteString("ready\n")
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

func startNonDumpable(t *testing.T) int {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root bypasses proc fd ownership checks")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSandboxFDHelperNonDumpable$")
	cmd.Env = append(os.Environ(), "LANTERN_FD_HELPER_NONDUMPABLE=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stdin.Close(); _ = cmd.Wait() })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "ready\n" {
		t.Fatalf("helper not ready: %q %v", line, err)
	}
	return cmd.Process.Pid
}

func TestTaskFDLiveUnreadableFailsClosed(t *testing.T) {
	pid := startNonDumpable(t)
	if _, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid)); !os.IsPermission(err) {
		t.Fatalf("precondition: live non-dumpable task fd dir error=%v, want EACCES", err)
	}
	root := t.TempDir()
	var window fdInspection
	ticker := time.NewTicker(samplePeriod)
	defer ticker.Stop()
	started := time.Now()
	observations := 0
	var reason error
	for reason == nil {
		now := <-ticker.C
		e := checkWorkspace(root, pid)
		if !errors.Is(e, errTaskFDNotReady) {
			t.Fatalf("observation %d: live unreadable task returned %v, want retryable not-ready", observations, e)
		}
		observations++
		reason = window.observe(now, e)
		if time.Since(started) > 5*time.Second {
			t.Fatalf("live unreadable task was not failed within 5s (%d observations)", observations)
		}
	}
	if observations < 2 || !strings.Contains(reason.Error(), "exiting=false") || !strings.Contains(reason.Error(), "task FD inspection temporarily unavailable") {
		t.Fatalf("observations=%d reason=%v", observations, reason)
	}
	if task, err := readTaskExit(pid); err != nil || task.tearingDown() {
		t.Fatalf("helper must still be live: %v %v", task, err)
	}
	t.Logf("failed closed after %d observations in %v: %v", observations, time.Since(started), reason)
}

// The exit_mm window grows with the address space, so a large single-threaded
// Python makes it observable with tight polling.
func TestTaskFDExitingTaskIsTeardown(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		if os.Getenv("SANDBOX_REQUIRE_REAL") == "1" {
			t.Fatal(err)
		}
		t.Skip("python3 unavailable")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses proc fd ownership checks")
	}
	root := t.TempDir()
	for attempt := 0; attempt < 5; attempt++ {
		cmd := exec.Command(python, "-c", "import sys,os\nb=bytearray(512<<20)\nfor i in range(0,len(b),4096): b[i]=1\nsys.stdout.write('ready\\n');sys.stdout.flush()\nsys.stdin.read(1)\nos._exit(0)")
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		pid := cmd.Process.Pid
		if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
			t.Fatalf("python not ready: %q %v", line, err)
		}
		if err = checkWorkspace(root, pid); err != nil {
			t.Fatalf("live dumpable task: %v", err)
		}
		stdin.Close()
		exitingDenied := 0
		for {
			_, rawErr := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
			before, statErr := readTaskExit(pid)
			if e := checkWorkspace(root, pid); e != nil {
				t.Fatalf("exiting task reported %v", e)
			}
			if statErr != nil || before.state == "Z" {
				break
			}
			if os.IsPermission(rawErr) && before.flags&pfExiting != 0 {
				exitingDenied++
			}
		}
		if err = cmd.Wait(); err != nil {
			t.Fatal(err)
		}
		if exitingDenied > 0 {
			t.Logf("attempt %d: %d EACCES observations of a live-state task with PF_EXITING, all treated as teardown", attempt, exitingDenied)
			return
		}
	}
	t.Fatal("never observed the exit_mm window; the test did not exercise the exiting path")
}

func TestFDInspectionWindow(t *testing.T) {
	notReady := fmt.Errorf("%w: open /proc/1/fd: permission denied [task state=S]", errTaskFDNotReady)
	t0 := time.Unix(0, 0)
	var w fdInspection
	if err := w.observe(t0, notReady); err != nil {
		t.Fatal(err)
	}
	if err := w.observe(t0.Add(samplePeriod), notReady); err != nil {
		t.Fatal(err)
	}
	err := w.observe(t0.Add(2*samplePeriod), notReady)
	if err == nil || !strings.Contains(err.Error(), "observations=3 over 20ms, max gap 10ms") {
		t.Fatalf("window did not fail closed with diagnostics: %v", err)
	}
	w = fdInspection{}
	_ = w.observe(t0, notReady)
	if err = w.observe(t0.Add(samplePeriod), nil); err != nil {
		t.Fatal(err)
	}
	if err = w.observe(t0.Add(3*samplePeriod), notReady); err != nil {
		t.Fatalf("readable observation must restart the window: %v", err)
	}
	if err = w.observe(t0, errors.New("workspace boom")); err == nil || !strings.Contains(err.Error(), "workspace boom") {
		t.Fatalf("non-retryable error must fail at once: %v", err)
	}
}

func TestTaskExitTearingDown(t *testing.T) {
	for _, c := range []struct {
		task taskExit
		want bool
	}{
		{taskExit{"R", 0x40000c, 1}, true},
		{taskExit{"D", pfExiting, 1}, true},
		{taskExit{"Z", 0, 1}, true},
		{taskExit{"X", 0, 0}, true},
		{taskExit{"R", 0x400000, 1}, false},
		{taskExit{"S", 0, 1}, false},
		{taskExit{"R", pfExiting, 2}, false},
		{taskExit{"Z", pfExiting, 3}, false},
	} {
		if got := c.task.tearingDown(); got != c.want {
			t.Errorf("%v tearingDown=%t want %t", c.task, got, c.want)
		}
	}
}
