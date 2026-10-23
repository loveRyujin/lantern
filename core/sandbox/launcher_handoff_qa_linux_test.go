//go:build linux && amd64

package sandbox_test

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/sandbox"
)

// This repeats the real process boundary while the host scheduler and GC are
// active. It is a stability regression, not a deterministic reproducer of the
// old post-TSYNC Go-runtime race; the controlled QA experiment records that
// separate evidence. Security assertions prevent fixing startup by allowing
// Python to fork or create threads.
func TestSandboxQALauncherConcurrentHandoff(t *testing.T) {
	blackboxRunner(t)
	stop := make(chan struct{})
	var pressure sync.WaitGroup
	var cycles atomic.Uint64
	for n := 0; n < 2; n++ {
		pressure.Add(1)
		go func() {
			defer pressure.Done()
			var live [16][]byte
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				live[i%len(live)] = make([]byte, 1<<10)
				live[i%len(live)][0] = byte(i)
				cycles.Add(1)
				runtime.Gosched()
				if i%64 == 0 {
					time.Sleep(time.Millisecond)
				}
			}
		}()
	}
	pressure.Add(1)
	go func() {
		defer pressure.Done()
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				runtime.GC()
			}
		}
	}()
	defer func() { close(stop); pressure.Wait() }()
	var runs sync.WaitGroup
	for worker := 0; worker < 2; worker++ {
		root := t.TempDir()
		runner := sandbox.New(sandbox.Config{TempRoot: root})
		runs.Add(1)
		go func(worker int) {
			defer runs.Done()
			for n := 0; n < 24; n++ {
				marker := fmt.Sprintf("handoff-%d-%d", worker, n)
				code := fmt.Sprintf("print(%q,flush=True)", marker)
				if n%8 == 0 {
					code = `import ctypes,errno,os
c=ctypes.CDLL(None,use_errno=True)
c.syscall.restype=ctypes.c_long
assert 'GODEBUG' not in os.environ
# Legacy fork, clone, clone3 and thread creation all remain denied.
# Any successful fork-like mutation child exits immediately; never multiply
# the test runner or leave test-induced descendants behind.
for nr in (57,56,435):
 r=c.syscall(nr,0,0,0,0,0,0)
 if r==0: os._exit(0)
 if r>0: os.waitpid(r,0)
 assert r==-1 and ctypes.get_errno()==errno.EPERM,(nr,r,ctypes.get_errno())
import threading
try:
 threading.Thread(target=lambda:None).start()
except RuntimeError: pass
else: raise AssertionError('Python thread was permitted')
` + code
				}
				ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
				result, err := runner.Run(ctx, sandbox.Spec{Code: code})
				cancel()
				if err != nil || result.Stdout != marker+"\n" || result.ErrKind != "" {
					t.Errorf("worker=%d run=%d result=%+v error=%v", worker, n, result, err)
					return
				}
				if result.Usage.PythonPID <= 0 {
					t.Errorf("no Python identity: %+v", result.Usage)
					return
				}
				if _, err := os.Stat(fmt.Sprintf("/proc/%d", result.Usage.PythonPID)); !os.IsNotExist(err) {
					t.Errorf("Python was not reaped: pid=%d error=%v", result.Usage.PythonPID, err)
					return
				}
				entries, err := os.ReadDir(root)
				if err != nil || len(entries) != 0 {
					t.Errorf("worker=%d cleanup entries=%v error=%v", worker, entries, err)
					return
				}
			}
		}(worker)
	}
	runs.Wait()
	if cycles.Load() == 0 {
		t.Fatal("host scheduling pressure did not run")
	}
	t.Logf("completed 48 real handoffs with host allocation/scheduling cycles=%d", cycles.Load())
}
