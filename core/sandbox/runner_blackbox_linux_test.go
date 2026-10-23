//go:build linux && amd64

package sandbox_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/errcode"
	"github.com/loveRyujin/lantern/core/pyrt"
	"github.com/loveRyujin/lantern/core/sandbox"
	"golang.org/x/sys/unix"
)

func blackboxRunner(t *testing.T) (*sandbox.Runner, string) {
	t.Helper()
	root := t.TempDir()
	runner := sandbox.New(sandbox.Config{TempRoot: root})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	result, err := runner.Run(ctx, sandbox.Spec{Code: "print('isolation-ready')"})
	if errors.Is(err, sandbox.ErrSandboxUnavailable) || errors.Is(err, sandbox.ErrRuntimeUnavailable) {
		if os.Getenv("SANDBOX_REQUIRE_REAL") != "1" {
			t.Skipf("real isolation unavailable: %v", err)
		}
	}
	if err != nil || strings.TrimSpace(result.Stdout) != "isolation-ready" {
		t.Fatalf("real sandbox preflight: result=%+v error=%v", result, err)
	}
	blackboxReaped(t, result.Usage.PythonPID)
	blackboxClean(t, root)
	return runner, root
}

func blackboxRun(t *testing.T, runner *sandbox.Runner, spec sandbox.Spec) (sandbox.Result, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	return runner.Run(ctx, spec)
}

func blackboxCode(t *testing.T, result sandbox.Result, err error, want errcode.Code) {
	t.Helper()
	code, ok := errcode.CodeOf(err)
	if !ok || code != want || result.ErrKind != want {
		t.Fatalf("result.ErrKind=%q error=%v code=%q/%v; want %q; result=%+v", result.ErrKind, err, code, ok, want, result)
	}
}

func blackboxReaped(t *testing.T, pid int) {
	t.Helper()
	if pid <= 0 || pid == os.Getpid() {
		t.Fatalf("invalid real Python PID: %d", pid)
	}
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); !os.IsNotExist(err) {
		t.Fatalf("Python %d remains after Run: %v", pid, err)
	}
}

func blackboxClean(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("execution directories were not cleaned: entries=%v error=%v", entries, err)
	}
}

func TestSandboxBlackboxSuccessFailureAndRecovery(t *testing.T) {
	runner, root := blackboxRunner(t)
	result, err := blackboxRun(t, runner, sandbox.Spec{Code: "import sys,time\nprint('stdout-marker')\nprint('stderr-marker',file=sys.stderr)\ntime.sleep(0.05)"})
	if err != nil || result.ErrKind != "" || result.Stdout != "stdout-marker\n" || result.Stderr != "stderr-marker\n" {
		t.Fatalf("stdout/stderr result=%+v error=%v", result, err)
	}
	if result.Usage.PeakRSS == 0 || result.Usage.SampleCount == 0 || result.Usage.Wall <= 0 {
		t.Fatalf("missing real usage evidence: %+v", result.Usage)
	}
	blackboxReaped(t, result.Usage.PythonPID)
	blackboxClean(t, root)
	result, err = blackboxRun(t, runner, sandbox.Spec{Code: "raise ValueError('controlled-failure')"})
	blackboxCode(t, result, err, errcode.Exec)
	blackboxReaped(t, result.Usage.PythonPID)
	blackboxClean(t, root)
	result, err = blackboxRun(t, runner, sandbox.Spec{Code: "print(42)"})
	if err != nil || strings.TrimSpace(result.Stdout) != "42" {
		t.Fatalf("query after failure: result=%+v error=%v", result, err)
	}
	blackboxClean(t, root)
}

func TestSandboxBlackboxFileNetworkAndEnvironment(t *testing.T) {
	runner, root := blackboxRunner(t)
	secret := filepath.Join(t.TempDir(), "workspace-sensitive.db")
	if err := os.WriteFile(secret, []byte("host-only-fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(secret); err != nil || string(data) != "host-only-fixture" {
		t.Fatalf("host read control: %q %v", data, err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	connection, err := net.DialTimeout("tcp4", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("host listener control: %v", err)
	}
	connection.Close()
	accepted, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	accepted.Close()
	t.Setenv("LANTERN_QA_SECRET", "must-not-enter-python")
	port := listener.Addr().(*net.TCPAddr).Port
	code := fmt.Sprintf(`import os,socket,sys
assert 'LANTERN_QA_SECRET' not in os.environ
assert open(os.path.join(os.environ['INPUT_DIR'],'data.csv')).read()=='value\n7\n'
for path in [%q, os.path.join(os.environ['INPUT_DIR'],'data.csv'), sys.executable]:
 try:
  if path==%q: open(path,'rb').read()
  else: open(path,'ab')
 except PermissionError: pass
 else: raise AssertionError('forbidden file access succeeded: '+path)
open(os.path.join(os.environ['OUTPUT_DIR'],'result.json'),'w').write('{"ok":true}')
for family,kind in [(socket.AF_INET,socket.SOCK_STREAM),(socket.AF_INET6,socket.SOCK_STREAM),(socket.AF_UNIX,socket.SOCK_STREAM),(socket.AF_NETLINK,socket.SOCK_RAW)]:
 try: socket.socket(family,kind)
 except PermissionError: pass
 else: raise AssertionError('socket allowed: '+str(family))
try: socket.create_connection(('127.0.0.1',%d),timeout=0.2)
except PermissionError: pass
else: raise AssertionError('host listener reached')
print('boundaries-verified')`, secret, secret, port)
	result, err := blackboxRun(t, runner, sandbox.Spec{Code: code, Inputs: map[string][]byte{"data.csv": []byte("value\n7\n")}})
	if err != nil || !strings.Contains(result.Stdout, "boundaries-verified") {
		t.Fatalf("boundaries result=%+v error=%v", result, err)
	}
	blackboxReaped(t, result.Usage.PythonPID)
	blackboxClean(t, root)
}

func TestSandboxBlackboxStrictSingleTaskAndHostSignals(t *testing.T) {
	runner, root := blackboxRunner(t)
	// Signal only our disposable helper; a removed guard cannot harm unrelated
	// processes. It records SIGUSR1 in a host-only file instead of terminating.
	marker := filepath.Join(t.TempDir(), "signal-received")
	helper := exec.Command("python3", "-c", `import signal,time,pathlib,sys
signal.signal(signal.SIGUSR1,lambda *_: pathlib.Path(sys.argv[1]).write_text('received'))
print('ready',flush=True)
time.sleep(20)`, marker)
	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = helper.Process.Kill(); _ = helper.Wait() }()
	ready := make([]byte, 6)
	if _, err := io.ReadFull(stdout, ready); err != nil || string(ready) != "ready\n" {
		t.Fatalf("host signal helper not ready: %q %v", ready, err)
	}
	// Positive control uses signal 0: the target exists and is ours to signal.
	if err := unix.Kill(helper.Process.Pid, 0); err != nil {
		t.Fatal(err)
	}
	var originalLimit unix.Rlimit
	if err := unix.Prlimit(helper.Process.Pid, unix.RLIMIT_NOFILE, nil, &originalLimit); err != nil {
		t.Fatal(err)
	}
	// A no-op host setter proves same-UID permission without altering the helper.
	if err := unix.Prlimit(helper.Process.Pid, unix.RLIMIT_NOFILE, &originalLimit, nil); err != nil {
		t.Fatalf("host prlimit setter positive control: %v", err)
	}
	var originalAffinity unix.CPUSet
	if err := unix.SchedGetaffinity(helper.Process.Pid, &originalAffinity); err != nil {
		t.Fatal(err)
	}
	if err := unix.SchedSetaffinity(helper.Process.Pid, &originalAffinity); err != nil {
		t.Fatalf("host affinity setter positive control: %v", err)
	}
	firstCPU := -1
	for i := 0; i < 1024; i++ {
		if originalAffinity.IsSet(i) {
			firstCPU = i
			break
		}
	}
	if firstCPU < 0 {
		t.Fatal("controlled helper has no allowed CPU")
	}
	code := fmt.Sprintf(`import os,threading,subprocess,ctypes,errno,resource
try:
 p=os.fork()
except PermissionError: pass
else:
 if p==0: os._exit(0)
 os.waitpid(p,0)
 raise AssertionError('fork allowed')
for action in [os.setsid,lambda:os.setpgid(0,0)]:
 try: action()
 except PermissionError: pass
 else: raise AssertionError('process group escape allowed')
try: threading.Thread(target=lambda:None).start()
except RuntimeError: pass
else: raise AssertionError('thread creation allowed')
try: subprocess.run(['/usr/bin/python3','-c','pass'],check=True)
except PermissionError: pass
else: raise AssertionError('subprocess allowed')
try: os.kill(%d,10)
except PermissionError: pass
else: raise AssertionError('host signal allowed')
for action in [lambda:resource.prlimit(%d,resource.RLIMIT_NOFILE,(16,%d)),lambda:os.sched_setaffinity(%d,{%d}),lambda:os.setpriority(os.PRIO_PROCESS,%d,19),lambda:os.setuid(os.getuid()),lambda:os.setgid(os.getgid())]:
 try: action()
 except PermissionError: pass
 else: raise AssertionError('host process or credential modification allowed')
libc=ctypes.CDLL(None,use_errno=True)
assert libc.prctl(1,0,0,0,0)==-1 and ctypes.get_errno()==errno.EPERM
print('single-task-verified')`, helper.Process.Pid, helper.Process.Pid, originalLimit.Max, helper.Process.Pid, firstCPU, helper.Process.Pid)
	result, err := blackboxRun(t, runner, sandbox.Spec{Code: code})
	if err != nil || !strings.Contains(result.Stdout, "single-task-verified") {
		t.Fatalf("strict profile result=%+v error=%v", result, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("sandbox affected host helper: %v", err)
	}
	var afterLimit unix.Rlimit
	var afterAffinity unix.CPUSet
	if err := unix.Prlimit(helper.Process.Pid, unix.RLIMIT_NOFILE, nil, &afterLimit); err != nil || afterLimit != originalLimit {
		t.Fatalf("host helper limits changed: before=%+v after=%+v error=%v", originalLimit, afterLimit, err)
	}
	if err := unix.SchedGetaffinity(helper.Process.Pid, &afterAffinity); err != nil || afterAffinity != originalAffinity {
		t.Fatalf("host helper affinity changed: error=%v", err)
	}
	blackboxClean(t, root)
}

func TestSandboxBlackboxInheritedDescriptors(t *testing.T) {
	runner, root := blackboxRunner(t)
	secret := filepath.Join(t.TempDir(), "descriptor-secret")
	if err := os.WriteFile(secret, []byte("fd-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(secret)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	fd, err := unix.FcntlInt(file.Fd(), unix.F_DUPFD, 256)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC != 0 {
		t.Fatalf("FD positive control is not inheritable: %d %v", flags, err)
	}
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(pair[0])
	defer unix.Close(pair[1])
	code := fmt.Sprintf(`import os,errno
for fd in [3,%d,%d,%d]:
 try: os.fstat(fd)
 except OSError as e: assert e.errno==errno.EBADF
 else: raise AssertionError('inherited host descriptor: '+str(fd))
assert os.read(0,1024)==b''
print('descriptors-closed')`, fd, pair[0], pair[1])
	result, err := blackboxRun(t, runner, sandbox.Spec{Code: code})
	if err != nil || !strings.Contains(result.Stdout, "descriptors-closed") {
		t.Fatalf("inherited descriptor result=%+v error=%v", result, err)
	}
	blackboxClean(t, root)
}

func TestSandboxBlackboxRSSAndVirtualReservation(t *testing.T) {
	runner, root := blackboxRunner(t)
	result, err := blackboxRun(t, runner, sandbox.Spec{Code: "import mmap,time\nx=mmap.mmap(-1,128<<20)\ntime.sleep(0.1)\nprint('virtual-reservation-ok')", Limits: sandbox.Limits{Memory: 32 << 20}})
	if err != nil || !strings.Contains(result.Stdout, "virtual-reservation-ok") {
		t.Fatalf("low-RSS virtual reservation must not be treated as RSS exhaustion: result=%+v error=%v", result, err)
	}
	if result.Usage.PeakRSS == 0 || result.Usage.PeakRSS >= 32<<20 {
		t.Fatalf("virtual reservation test did not stay under RSS budget: %+v", result.Usage)
	}
	result, err = blackboxRun(t, runner, sandbox.Spec{Code: "import time\nx=bytearray(64<<20)\ntime.sleep(5)", Limits: sandbox.Limits{Memory: 32 << 20}})
	blackboxCode(t, result, err, errcode.OOM)
	if result.Usage.PeakRSS <= 32<<20 || result.Usage.SampleCount == 0 || result.Usage.MaxSampleGap <= 0 {
		t.Fatalf("missing measured RSS violation/window evidence: %+v", result.Usage)
	}
	blackboxReaped(t, result.Usage.PythonPID)
	blackboxClean(t, root)
}

func TestSandboxBlackboxCPUAndWallTimeout(t *testing.T) {
	runner, root := blackboxRunner(t)
	result, err := blackboxRun(t, runner, sandbox.Spec{Code: "import time\ntime.sleep(0.4)\nprint('sleep-ok')", Limits: sandbox.Limits{CPU: 200 * time.Millisecond, Wall: 3 * time.Second}})
	if err != nil || !strings.Contains(result.Stdout, "sleep-ok") {
		t.Fatalf("CPU time must not mean wall time: result=%+v error=%v", result, err)
	}
	result, err = blackboxRun(t, runner, sandbox.Spec{Code: "while True: pass", Limits: sandbox.Limits{CPU: 200 * time.Millisecond, Wall: 8 * time.Second}})
	blackboxCode(t, result, err, errcode.Timeout)
	if result.Usage.CPU < 200*time.Millisecond || result.Usage.Wall >= 5*time.Second {
		t.Fatalf("CPU budget was not independently enforced: %+v", result.Usage)
	}
	blackboxReaped(t, result.Usage.PythonPID)
	result, err = blackboxRun(t, runner, sandbox.Spec{Code: "import time\nprint('wall-payload-started',flush=True)\ntime.sleep(20)", Limits: sandbox.Limits{Wall: 2 * time.Second}})
	blackboxCode(t, result, err, errcode.Timeout)
	if !strings.Contains(result.Stdout, "wall-payload-started") {
		t.Fatalf("wall test timed out before actual Python execution: %+v", result)
	}
	blackboxReaped(t, result.Usage.PythonPID)
	blackboxClean(t, root)
}

func blackboxWaitMarker(t *testing.T, root, name string, done <-chan error) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()
	for {
		var pid int
		_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err == nil && entry.Name() == name && !entry.IsDir() {
				if body, err := os.ReadFile(path); err == nil {
					pid, _ = strconv.Atoi(strings.TrimSpace(string(body)))
				}
			}
			return nil
		})
		if pid > 0 {
			return pid
		}
		select {
		case err := <-done:
			t.Fatalf("execution ended before real Python marker: %v", err)
		case <-ctx.Done():
			t.Fatalf("waiting for actual Python execution: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestSandboxBlackboxCancellationAndConcurrentRun(t *testing.T) {
	runner, root := blackboxRunner(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	aDone := make(chan error, 1)
	var aResult sandbox.Result
	go func() {
		var err error
		aResult, err = runner.Run(ctx, sandbox.Spec{Code: "import os,time\nopen(os.environ['TMPDIR']+'/cancel-ready','w').write(str(os.getpid()))\ntime.sleep(20)"})
		aDone <- err
	}()
	pid := blackboxWaitMarker(t, root, "cancel-ready", aDone)
	bDone := make(chan error, 1)
	var bResult sandbox.Result
	go func() {
		var err error
		bResult, err = runner.Run(t.Context(), sandbox.Spec{Code: "import os,time\nopen(os.environ['TMPDIR']+'/other-ready','w').write(str(os.getpid()))\ntime.sleep(0.2)\nprint('other-survived')"})
		bDone <- err
	}()
	other := blackboxWaitMarker(t, root, "other-ready", bDone)
	if pid == other {
		t.Fatal("concurrent runs share a Python process")
	}
	cancel()
	select {
	case err := <-aDone:
		blackboxCode(t, aResult, err, errcode.Timeout)
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not terminate promptly")
	}
	if err := <-bDone; err != nil || !strings.Contains(bResult.Stdout, "other-survived") {
		t.Fatalf("cancellation crossed execution boundary: result=%+v error=%v", bResult, err)
	}
	blackboxReaped(t, pid)
	blackboxReaped(t, other)
	blackboxClean(t, root)
}

func TestSandboxBlackboxParentDeathHelper(t *testing.T) {
	if os.Getenv("LANTERN_QA_PARENT_DEATH_HELPER") != "1" {
		return
	}
	runner := sandbox.New(sandbox.Config{TempRoot: os.Getenv("LANTERN_QA_RUN_ROOT")})
	_, err := runner.Run(context.Background(), sandbox.Spec{Code: "import os,time\nopen(os.environ['TMPDIR']+'/parent-death-ready','w').write(str(os.getpid()))\ntime.sleep(60)", Limits: sandbox.Limits{Wall: 60 * time.Second}})
	t.Fatalf("parent-death helper Run returned before host was killed: %v", err)
}

func TestSandboxBlackboxParentDeathReapsAndCleans(t *testing.T) {
	_, _ = blackboxRunner(t)
	root := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	helper := exec.CommandContext(ctx, exe, "-test.run=^TestSandboxBlackboxParentDeathHelper$")
	helper.Env = append(os.Environ(), "LANTERN_QA_PARENT_DEATH_HELPER=1", "LANTERN_QA_RUN_ROOT="+root)
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- helper.Wait() }()
	defer func() { _ = helper.Process.Kill() }()
	pid := blackboxWaitMarker(t, root, "parent-death-ready", done)
	if err := helper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-done
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, processErr := os.Stat(fmt.Sprintf("/proc/%d", pid))
		entries, dirErr := os.ReadDir(root)
		if os.IsNotExist(processErr) && dirErr == nil && len(entries) == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	blackboxReaped(t, pid)
	blackboxClean(t, root)
}

func TestSandboxBlackboxInvalidProfileAndMissingRuntime(t *testing.T) {
	root := t.TempDir()
	runner := sandbox.New(sandbox.Config{TempRoot: root})
	for _, limits := range []sandbox.Limits{{NProc: 2}, {Memory: 1<<30 + 1}, {CPU: 31 * time.Second}, {Wall: 61 * time.Second}} {
		if _, err := blackboxRun(t, runner, sandbox.Spec{Code: "raise AssertionError('invalid request executed')", Limits: limits}); !errors.Is(err, sandbox.ErrInvalidSpec) {
			t.Fatalf("limits=%+v error=%v; want ErrInvalidSpec", limits, err)
		}
	}
	result, err := blackboxRun(t, runner, sandbox.Spec{Code: "raise AssertionError('missing runtime fallback')", Runtime: sandbox.Runtime{Python: filepath.Join(t.TempDir(), "missing-python")}})
	blackboxCode(t, result, err, errcode.RuntimeUnavailable)
	blackboxClean(t, root)
}

func TestSandboxBlackboxOutputAndWorkspaceBudgets(t *testing.T) {
	runner, root := blackboxRunner(t)
	for _, fd := range []int{1, 2} {
		t.Run(fmt.Sprintf("stream-%d", fd), func(t *testing.T) {
			result, err := blackboxRun(t, runner, sandbox.Spec{Code: fmt.Sprintf("import os,time\nos.write(%d,b'x'*(128<<10))\ntime.sleep(0.2)", fd)})
			blackboxCode(t, result, err, errcode.Exec)
			if len(result.Stdout) > 64<<10 || len(result.Stderr) > 64<<10 {
				t.Fatalf("stream cap exceeded: stdout=%d stderr=%d", len(result.Stdout), len(result.Stderr))
			}
			blackboxReaped(t, result.Usage.PythonPID)
			blackboxClean(t, root)
		})
	}
	for _, test := range []struct{ name, code string }{
		{"workspace-total", "import os,time\nchunk=b'x'*(4<<20)\nfor i in range(20):\n with open(os.environ['TMPDIR']+'/'+str(i),'wb') as f: f.write(chunk)\ntime.sleep(1)"},
		{"workspace-entries", "import os,time\nfor i in range(1100): open(os.environ['TMPDIR']+'/'+str(i),'w').close()\ntime.sleep(1)"},
		{"deleted-open-total", "import os,time\nfds=[]\nchunk=b'x'*(4<<20)\nfor i in range(20):\n p=os.environ['TMPDIR']+'/'+str(i)\n f=open(p,'wb'); fds.append(f); os.unlink(p); f.write(chunk); f.flush()\ntime.sleep(1)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := blackboxRun(t, runner, sandbox.Spec{Code: test.code})
			blackboxCode(t, result, err, errcode.Exec)
			blackboxReaped(t, result.Usage.PythonPID)
			blackboxClean(t, root)
		})
	}
}

func TestSandboxBlackboxRealPackages(t *testing.T) {
	if os.Getenv("SANDBOX_REAL_PACKAGES") != "1" {
		t.Skip("hash-locked pandas/matplotlib integration requires SANDBOX_REAL_PACKAGES=1; default tests do not install packages")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Minute)
	defer cancel()
	prepared, err := (pyrt.Config{Root: t.TempDir()}).Ensure(ctx)
	if err != nil {
		t.Fatalf("required real venv preparation failed: %v", err)
	}
	root := t.TempDir()
	runner := sandbox.New(sandbox.Config{TempRoot: root})
	result, err := runner.Run(ctx, sandbox.Spec{Runtime: sandbox.Runtime{Python: prepared.Python, PyRoot: prepared.Base.Stdlib, Venv: prepared.Venv}, Code: `import os,pandas as pd,matplotlib
matplotlib.use('Agg')
import matplotlib.pyplot as plt
frame=pd.DataFrame({'x':[1,2,3],'y':[2,4,6]})
assert frame['y'].sum()==12
fig,ax=plt.subplots()
ax.plot(frame['x'],frame['y'])
fig.savefig(os.path.join(os.environ['OUTPUT_DIR'],'chart.png'))
print('pandas-result:12')`})
	if err != nil || !strings.Contains(result.Stdout, "pandas-result:12") {
		t.Fatalf("required real-package sandbox execution: result=%+v error=%v", result, err)
	}
	found := false
	for _, artifact := range result.Artifacts {
		if artifact.Name == "chart.png" {
			found = true
			if artifact.Kind != sandbox.KindPNG || sandbox.ValidateArtifact(artifact, 0) != nil {
				t.Fatalf("invalid real PNG artifact: name=%s kind=%s bytes=%d", artifact.Name, artifact.Kind, len(artifact.Data))
			}
		}
	}
	if !found {
		t.Fatal("sandbox did not return the generated PNG before cleaning execution files")
	}
	blackboxReaped(t, result.Usage.PythonPID)
	blackboxClean(t, root)
}
