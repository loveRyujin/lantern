//go:build linux && amd64

package eval

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/loveRyujin/lantern/core/errcode"
	"github.com/loveRyujin/lantern/core/pyrt"
	"github.com/loveRyujin/lantern/core/sandbox"
	"golang.org/x/sys/unix"
)

const realEntered = "LANTERN_REAL_ENTERED"
const realPacket = "LANTERN_REAL_JSON:"

type realAttemptPacket struct {
	Name      string `json:"name"`
	Attempted bool   `json:"attempted"`
	Success   bool   `json:"success"`
	Errno     *int   `json:"errno"`
}
type realPayloadPacket struct {
	Operations []realAttemptPacket `json:"operations"`
}
type linuxRealExecutor struct {
	runtime pyrt.Runtime
	root    string
}

func newSandboxRealExecutor(ctx context.Context, rt pyrt.Runtime) (sandboxRealExecutor, map[string]any, error) {
	var uts unix.Utsname
	_ = unix.Uname(&uts)
	kernel := strings.TrimRight(string(uts.Release[:]), "\x00")
	env := map[string]any{"os": "linux", "arch": "amd64", "kernel": kernel, "uid": os.Getuid(), "euid": os.Geteuid(), "python_version": rt.Base.Version.String(), "implementation": "production sandbox.Run; Landlock + thread-scoped seccomp; strict single task", "rss_accounting": "10ms sampled task RSS; not a kernel hard RSS quota"}
	env["units"] = map[string]string{"CPU": "nanoseconds", "Wall": "nanoseconds", "MaxSampleGap": "nanoseconds", "Memory": "bytes", "RSS": "bytes", "MaxOutput": "bytes"}
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var caps [2]unix.CapUserData
	capErr := unix.Capget(&header, &caps[0])
	effective := uint64(caps[0].Effective) | uint64(caps[1].Effective)<<32
	permitted := uint64(caps[0].Permitted) | uint64(caps[1].Permitted)<<32
	env["cap_effective_hex"] = fmt.Sprintf("%x", effective)
	env["cap_permitted_hex"] = fmt.Sprintf("%x", permitted)
	abi, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	env["landlock_abi"] = nil
	if errno == 0 {
		env["landlock_abi"] = uint64(abi)
	} else {
		env["landlock_raw_return"] = fmt.Sprintf("%#x", abi)
	}
	if errno != 0 {
		env["landlock_errno"] = int(errno)
	}
	if ctx.Err() != nil {
		return nil, env, ctx.Err()
	}
	if capErr != nil || os.Getuid() == 0 || os.Geteuid() == 0 || effective != 0 || permitted != 0 || errno != 0 || abi < 3 {
		return nil, env, fmt.Errorf("real sandbox requires non-root/capability-free Linux amd64 with Landlock ABI >=3")
	}
	root, err := os.MkdirTemp("", "lantern-real-eval-")
	if err != nil {
		return nil, env, err
	}
	executor := &linuxRealExecutor{rt, root}
	preflightRoot := filepath.Join(root, "preflight")
	if err = os.Mkdir(preflightRoot, 0700); err != nil {
		_ = executor.Close()
		return nil, env, err
	}
	result, runErr := sandbox.New(sandbox.Config{TempRoot: preflightRoot}).Run(ctx, executor.spec("print('LANTERN_REAL_PREFLIGHT',flush=True)"))
	env["preflight_python_pid"] = result.Usage.PythonPID
	_, procErr := os.Stat(fmt.Sprintf("/proc/%d", result.Usage.PythonPID))
	entries, readErr := os.ReadDir(preflightRoot)
	reaped := result.Usage.PythonPID > 0 && os.IsNotExist(procErr)
	clean := readErr == nil && len(entries) == 0
	env["preflight_process_reaped"] = reaped
	env["preflight_task_cleanup"] = clean
	if runErr != nil || !strings.Contains(result.Stdout, "LANTERN_REAL_PREFLIGHT") || !reaped || !clean {
		_ = executor.Close()
		return nil, env, fmt.Errorf("normal production Runner preflight failed: %v", runErr)
	}
	env["normal_production_preflight"] = "passed"
	return executor, env, nil
}
func (x *linuxRealExecutor) Close() error { return os.RemoveAll(x.root) }
func (x *linuxRealExecutor) CleanupComplete() bool {
	_, err := os.Lstat(x.root)
	return os.IsNotExist(err)
}
func (x *linuxRealExecutor) spec(code string) sandbox.Spec {
	return sandbox.Spec{Code: code, Limits: sandbox.Limits{CPU: 10 * time.Second, Wall: 30 * time.Second, Memory: 512 << 20, NProc: 1, MaxOutput: 5 << 20}, Runtime: sandbox.Runtime{Python: x.runtime.Python, PyRoot: x.runtime.Base.Stdlib, Venv: x.runtime.Venv}}
}
func realEvidence() SandboxEvidence {
	return SandboxEvidence{Operations: []SandboxOperation{}, Observations: map[string]any{}}
}
func realFailure(e SandboxEvidence, err error) SandboxEvidence { e.Detail = err.Error(); return e }
func (e *SandboxEvidence) observe(key string, value any)       { e.Observations[key] = value }
func realHostAttempt(fn func() error) *SandboxAttempt {
	err := fn()
	r := &SandboxAttempt{Attempted: true, Success: err == nil}
	if err != nil {
		var errno syscallErrno
		if errors.As(err, &errno) {
			n := int(errno)
			r.Errno = &n
		}
		r.Detail = err.Error()
	}
	return r
}

// syscall.Errno and unix.Errno alias the same platform errno type.
type syscallErrno = unix.Errno

func realSocket(family, kind, protocol int) error {
	fd, err := unix.Socket(family, kind, protocol)
	if fd >= 0 {
		_ = unix.Close(fd)
	}
	return err
}
func realPrelude() string {
	return `import os,sys,json,errno,socket,time,ctypes
print('LANTERN_REAL_ENTERED',flush=True)
_real_operations=[]
def _real_attempt(name,action):
 try:
  value=action()
  if hasattr(value,'close'): value.close()
  _real_operations.append(dict(name=name,attempted=True,success=True,errno=None))
 except OSError as error:
  while isinstance(getattr(error,'reason',None),BaseException): error=error.reason
  _real_operations.append(dict(name=name,attempted=True,success=False,errno=getattr(error,'errno',None)))
def _real_finish():
 print('LANTERN_REAL_JSON:'+json.dumps(dict(operations=_real_operations)),flush=True)
`
}
func (x *linuxRealExecutor) run(ctx context.Context, id string, spec sandbox.Spec, e SandboxEvidence) (sandbox.Result, error, SandboxEvidence) {
	dir := filepath.Join(x.root, "runs", id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return sandbox.Result{}, err, realFailure(e, err)
	}
	sum := sha256.Sum256([]byte(spec.Code))
	e.PayloadSHA256 = hex.EncodeToString(sum[:])
	result, err := sandbox.New(sandbox.Config{TempRoot: dir}).Run(ctx, spec)
	e.Limits = result.Limits
	e.Usage = result.Usage
	e.PythonPID = result.Usage.PythonPID
	e.PayloadEntered = strings.Contains(result.Stdout, realEntered)
	e.ErrorCode = "none"
	if err != nil {
		if code, ok := errcode.CodeOf(err); ok {
			e.ErrorCode = string(code)
		} else {
			e.ErrorCode = "uncoded_error"
		}
	}
	if e.PythonPID > 0 {
		_, procErr := os.Stat(fmt.Sprintf("/proc/%d", e.PythonPID))
		e.ProcessReaped = os.IsNotExist(procErr)
	}
	entries, dirErr := os.ReadDir(dir)
	e.TempClean = dirErr == nil && len(entries) == 0
	e.observe("payload_entered", e.PayloadEntered)
	e.observe("worker_started", e.PythonPID > 0)
	e.observe("process_reaped", e.ProcessReaped)
	e.observe("temp_clean", e.TempClean)
	e.observe("error_code", e.ErrorCode)
	return result, err, e
}
func realParseOperations(result sandbox.Result, e *SandboxEvidence) error {
	index := strings.LastIndex(result.Stdout, realPacket)
	if index < 0 {
		return fmt.Errorf("real payload did not emit its operation observations")
	}
	text := strings.SplitN(result.Stdout[index+len(realPacket):], "\n", 2)[0]
	var packet realPayloadPacket
	d := json.NewDecoder(strings.NewReader(text))
	d.DisallowUnknownFields()
	if err := d.Decode(&packet); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, op := range packet.Operations {
		if seen[op.Name] || !op.Attempted {
			return fmt.Errorf("invalid/duplicate operation evidence")
		}
		seen[op.Name] = true
		matched := false
		for i := range e.Operations {
			if e.Operations[i].Name == op.Name {
				e.Operations[i].Sandbox = &SandboxAttempt{Attempted: op.Attempted, Success: op.Success, Errno: op.Errno}
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("unexpected operation %s", op.Name)
		}
	}
	for _, op := range e.Operations {
		if op.Sandbox == nil {
			return fmt.Errorf("missing operation %s", op.Name)
		}
	}
	return nil
}
func realCheckDenials(e *SandboxEvidence) {
	hostOK, denyOK := true, true
	for _, op := range e.Operations {
		if op.Attribution == "actual-host-target-open-only" {
			if op.Sandbox == nil || !op.Sandbox.Attempted || op.Sandbox.Success {
				denyOK = false
			}
			continue
		}
		if op.Attribution == "sandbox-write-positive-control" {
			if op.Sandbox == nil || !op.Sandbox.Attempted || !op.Sandbox.Success {
				denyOK = false
			}
			continue
		}
		hostAllowedFailure := op.Attribution == "ambient-denial-no-independent-attribution" || op.Attribution == "invalid-ptrace-request-control" && op.Host != nil && op.Host.Errno != nil && (*op.Host.Errno == int(unix.ESRCH) || *op.Host.Errno == int(unix.EIO))
		if !hostAllowedFailure && (op.Host == nil || !op.Host.Attempted || !op.Host.Success) {
			hostOK = false
		}
		if op.Sandbox == nil || !op.Sandbox.Attempted || op.Sandbox.Success || op.Sandbox.Errno == nil || (*op.Sandbox.Errno != int(unix.EACCES) && *op.Sandbox.Errno != int(unix.EPERM)) {
			denyOK = false
		}
	}
	e.observe("required_host_controls", hostOK)
	e.observe("sandbox_denials", denyOK)
}
func realChildProcessGroup() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }
func realPythonString(value string) string        { data, _ := json.Marshal(value); return string(data) }
func realConn(address, network string) error {
	conn, err := net.DialTimeout(network, address, time.Second)
	if err == nil {
		_ = conn.Close()
	}
	return err
}
func (x *linuxRealExecutor) Execute(ctx context.Context, item Case) SandboxEvidence {
	switch item.ID {
	case "SB-LINUX-01", "SB-LINUX-02", "SB-LINUX-03", "SB-LINUX-04", "SB-LINUX-05":
		return x.network(ctx, item)
	case "SB-LINUX-06", "SB-LINUX-07", "SB-LINUX-08", "SB-LINUX-09", "SB-LINUX-10", "SB-LINUX-11":
		return x.filesystem(ctx, item)
	case "SB-LINUX-12", "SB-LINUX-13", "SB-LINUX-17", "SB-LINUX-18":
		return x.process(ctx, item)
	case "SB-LINUX-14", "SB-LINUX-15", "SB-LINUX-16", "SB-LINUX-19", "SB-LINUX-21":
		return x.resource(ctx, item)
	case "SB-LINUX-20":
		return x.networkRequest(ctx, item)
	case "SB-LINUX-22":
		return x.parentDeath(ctx, item)
	case "SB-LINUX-23":
		return x.unavailable(ctx, item)
	case "SB-LINUX-24":
		return x.productionPNG(ctx, item)
	case "SB-LINUX-25":
		return x.cancellation(ctx, item)
	default:
		return realFailure(realEvidence(), fmt.Errorf("unimplemented real case %s", item.ID))
	}
}
func realDataHash(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }
func realPacketLine(out []byte) []byte {
	if i := bytes.IndexByte(out, '\n'); i >= 0 {
		return out[:i]
	}
	return out
}
