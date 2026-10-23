//go:build linux && amd64

package sandbox_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/loveRyujin/lantern/core/sandbox"
)

// These setters do not actually send a signal: each operation must fail before
// any I/O is triggered. A mutation therefore fails an assertion, not a host task.
func TestSandboxFcntlAsyncSignalBoundary(t *testing.T) {
	runner, root := blackboxRunner(t)
	result, err := blackboxRun(t, runner, sandbox.Spec{Code: `import os,fcntl,struct,errno
r,w=os.pipe()
flags=fcntl.fcntl(w,fcntl.F_GETFL)
fcntl.fcntl(w,fcntl.F_SETFL,flags|os.O_NONBLOCK)
assert not os.get_blocking(w)
checks=[lambda:fcntl.fcntl(w,fcntl.F_SETOWN,os.getppid()),
 lambda:fcntl.fcntl(w,15,struct.pack('ii',1,os.getppid())),
 lambda:fcntl.fcntl(w,fcntl.F_SETSIG,10),
 lambda:fcntl.fcntl(w,fcntl.F_SETFL,flags|os.O_ASYNC),
 lambda:fcntl.ioctl(w,0x5452,struct.pack('i',1))]
for action in checks:
 try: action()
 except OSError as e: assert e.errno==errno.EPERM,e
 else: raise AssertionError('async host-signal configuration allowed')
os.close(r);os.close(w)
print('async-signal-blocked')`})
	if err != nil || result.Stdout != "async-signal-blocked\n" {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	blackboxClean(t, root)
}

func TestSandboxInputEntryBudgets(t *testing.T) {
	runner := sandbox.New(sandbox.Config{TempRoot: t.TempDir()})
	inputs := make(map[string][]byte)
	for i := 0; i < 1025; i++ {
		inputs[fmt.Sprintf("empty-%d", i)] = nil
	}
	for _, items := range []map[string][]byte{inputs, {"__code__.py/nested": nil}} {
		_, err := runner.Run(t.Context(), sandbox.Spec{Code: "raise AssertionError('invalid inputs executed')", Inputs: items})
		if !errors.Is(err, sandbox.ErrInvalidSpec) {
			t.Fatalf("want ErrInvalidSpec: %v", err)
		}
	}
}

func TestSandboxQuickExitProcfsRace(t *testing.T) {
	runner, root := blackboxRunner(t)
	for i := 0; i < 20; i++ {
		result, err := blackboxRun(t, runner, sandbox.Spec{Code: "print('quick-exit')"})
		if err != nil || result.Stdout != "quick-exit\n" {
			t.Fatalf("run %d: result=%+v error=%v", i, result, err)
		}
		blackboxReaped(t, result.Usage.PythonPID)
		blackboxClean(t, root)
	}
}

func TestSandboxRuntimeDirectoriesCannotExpandReadAccess(t *testing.T) {
	runner, root := blackboxRunner(t)
	unrelated := t.TempDir()
	for _, rt := range []sandbox.Runtime{{PyRoot: unrelated}, {Venv: unrelated}} {
		result, err := blackboxRun(t, runner, sandbox.Spec{Code: "raise AssertionError('unrelated runtime directory accepted')", Runtime: rt})
		if !errors.Is(err, sandbox.ErrRuntimeUnavailable) {
			t.Fatalf("result=%+v error=%v; want runtime_unavailable", result, err)
		}
		blackboxClean(t, root)
	}
}
