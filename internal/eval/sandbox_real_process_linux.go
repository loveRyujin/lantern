//go:build linux && amd64

package eval

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"golang.org/x/sys/unix"
)

const realForkProbe = `def _bounded_fork():
 pid=os.fork()
 if pid==0: os._exit(0)
 os.waitpid(pid,0)
 return pid
`
const realThreadProbe = `def _bounded_thread():
 libc=ctypes.CDLL(None)
 callback_type=ctypes.CFUNCTYPE(ctypes.c_void_p,ctypes.c_void_p)
 callback=callback_type(lambda unused:None)
 thread=ctypes.c_ulong()
 libc.pthread_create.argtypes=[ctypes.POINTER(ctypes.c_ulong),ctypes.c_void_p,callback_type,ctypes.c_void_p]
 libc.pthread_join.argtypes=[ctypes.c_ulong,ctypes.c_void_p]
 result=libc.pthread_create(ctypes.byref(thread),None,callback,None)
 if result: raise OSError(result,'pthread_create')
 result=libc.pthread_join(thread,None)
 if result: raise OSError(result,'pthread_join')
 return 0
`

func (x *linuxRealExecutor) hostPython(ctx context.Context, code string) error {
	command := exec.CommandContext(ctx, x.runtime.Python, "-I", "-B", "-c", "import os,ctypes\n"+code)
	command.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + x.root, "PYTHONDONTWRITEBYTECODE=1", "LANG=C.UTF-8"}
	return command.Run()
}

func (x *linuxRealExecutor) process(ctx context.Context, item Case) SandboxEvidence {
	e := realEvidence()
	var body string
	switch item.ID {
	case "SB-LINUX-12":
		host := realHostAttempt(func() error { return x.hostPython(ctx, realForkProbe+"_bounded_fork()\n") })
		e.Operations = []SandboxOperation{{Name: "bounded-fork-attempt", Host: host}, {Name: "pthread-create", Host: realHostAttempt(func() error { return x.hostPython(ctx, realThreadProbe+"_bounded_thread()\n") })}}
		body = realForkProbe + realThreadProbe + "_real_attempt('bounded-fork-attempt',_bounded_fork)\n_real_attempt('pthread-create',_bounded_thread)\n"
	case "SB-LINUX-13":
		host := realHostAttempt(func() error { return exec.CommandContext(ctx, "/usr/bin/setsid", "/usr/bin/true").Run() })
		e.Operations = []SandboxOperation{{Name: "setsid", Host: host}, {Name: "setpgid", Host: realHostAttempt(func() error {
			cmd := exec.CommandContext(ctx, "/usr/bin/true")
			cmd.SysProcAttr = realChildProcessGroup()
			return cmd.Run()
		})}, {Name: "bounded-fork-attempt", Host: realHostAttempt(func() error { return x.hostPython(ctx, realForkProbe+"_bounded_fork()\n") })}}
		body = realForkProbe + `_real_attempt('setsid',os.setsid)
_real_attempt('setpgid',lambda:os.setpgid(0,0))
_real_attempt('bounded-fork-attempt',_bounded_fork)
`
	case "SB-LINUX-17":
		e.Operations = []SandboxOperation{{Name: "setuid-self", Host: realHostAttempt(func() error { return unix.Setuid(os.Getuid()) })}}
		body = "_real_attempt('setuid-self',lambda:os.setuid(os.getuid()))\n"
	case "SB-LINUX-18":
		host := realHostAttempt(func() error {
			_, _, e := unix.Syscall6(unix.SYS_PTRACE, ^uintptr(0), uintptr(os.Getpid()), 0, 0, 0, 0)
			if e != 0 {
				return e
			}
			return nil
		})
		e.Operations = []SandboxOperation{{Name: "ptrace-invalid-host-request", Host: host, Attribution: "invalid-ptrace-request-control"}}
		// Request -1 is never TRACEME or ATTACH: even a mutation cannot stop a
		// guardian or change process tracing state. The filter must preempt it with EPERM.
		body = fmt.Sprintf(`def _ptrace_invalid():
 libc=ctypes.CDLL(None,use_errno=True)
 result=libc.ptrace(ctypes.c_ulong(-1),ctypes.c_ulong(%d),None,None)
 if result==-1:
  error=ctypes.get_errno()
  raise OSError(error,'ptrace invalid request')
 return result
_real_attempt('ptrace-invalid-host-request',_ptrace_invalid)
`, os.Getpid())
	}
	result, _, e := x.run(ctx, item.ID, x.spec(realPrelude()+body+"_real_finish()\n"), e)
	if err := realParseOperations(result, &e); err != nil {
		return realFailure(e, err)
	}
	realCheckDenials(&e)
	if item.ID == "SB-LINUX-12" || item.ID == "SB-LINUX-13" {
		children := 0
		for _, op := range e.Operations {
			if op.Name == "bounded-fork-attempt" && op.Sandbox != nil && op.Sandbox.Success {
				children++
			}
		}
		e.observe("created_children", children)
	}
	return e
}
