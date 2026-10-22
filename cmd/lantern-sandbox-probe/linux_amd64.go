//go:build linux && amd64

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const workerFlag = "--trusted-launcher"
const checksFlag = "--fixed-checks"

var runtimeRoots = []string{"/usr", "/lib", "/lib64"}

func run(args []string) report {
	if len(args) == 2 && args[0] == workerFlag {
		return launch(args[1])
	}
	if len(args) == 2 && args[0] == checksFlag {
		return checks(args[1])
	}
	r := report{Backend: "landlock-seccomp", Status: "failed", Kernel: kernel(), RuntimeRoots: runtimeRoots}
	if len(args) != 0 {
		r.Error = "no user command/code accepted"
		return r
	}
	abi, err := landlockABI()
	r.ABI = abi
	if err != nil || abi < 3 {
		r.Status = "unsupported"
		r.Error = fmt.Sprintf("Landlock ABI >=3 required: ABI=%d error=%v; no payload executed", abi, err)
		return r
	}
	root, err := os.MkdirTemp("", "lantern-sandbox-probe-")
	if err != nil {
		r.Error = err.Error()
		return r
	}
	defer os.RemoveAll(root)
	for _, d := range []string{"input", "runtime", "out", "tmp"} {
		if err = os.Mkdir(filepath.Join(root, d), 0700); err != nil {
			r.Error = err.Error()
			return r
		}
	}
	for _, p := range []string{"input/data", "runtime/data", "outside"} {
		if err = os.WriteFile(filepath.Join(root, p), []byte("fixture"), 0600); err != nil {
			r.Error = err.Error()
			return r
		}
	}
	self, err := os.Executable()
	if err != nil {
		r.Error = err.Error()
		return r
	}
	binary, err := os.ReadFile(self)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	exe := filepath.Join(root, "runtime/probe")
	if err = os.WriteFile(exe, binary, 0700); err != nil {
		r.Error = err.Error()
		return r
	}
	controls := hostControls(root)
	environment := hostEnvironment()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, workerFlag, root)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "PYTHONDONTWRITEBYTECODE=1", "TMPDIR=" + filepath.Join(root, "tmp")}
	cmd.Stdin = nil
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, runErr := cmd.Output()
	if err = json.Unmarshal(output, &r); err != nil {
		r.Status = "failed"
		r.Error = fmt.Sprintf("worker failed: %v, JSON: %v, stderr: %.500s", runErr, err, stderr.String())
	}
	if runErr != nil && r.Status == "passed" {
		r.Status = "failed"
		r.Error = fmt.Sprintf("worker returned passing JSON but exited with error: %v", runErr)
	}
	r.Controls = controls
	if r.Environment == nil {
		r.Environment = map[string]any{}
	}
	r.Environment["parent"] = environment
	for _, c := range controls {
		if !c.Passed {
			r.Status = "failed"
			r.Error = "host positive control failed; isolation result inconclusive"
		}
	}
	// Unsupported security installation is a negative result, never an execution fallback.
	return r
}
func kernel() string {
	var u unix.Utsname
	if unix.Uname(&u) != nil {
		return "unknown"
	}
	var b []byte
	for _, c := range u.Release {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}
func landlockABI() (int, error) {
	v, _, e := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if e != 0 {
		return 0, e
	}
	return int(v), nil
}
func launch(root string) report {
	r := report{Backend: "landlock-seccomp", Status: "unsupported", Kernel: kernel(), RuntimeRoots: runtimeRoots}
	r.ABI, _ = landlockABI()
	runtime.LockOSThread() // Landlock restricts this thread; exec must occur on it.
	// Never unlock: on any setup failure this subprocess exits without a payload.
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		r.Error = "no_new_privs: " + err.Error()
		return r
	}
	if err := restrictFiles(root); err != nil {
		r.Error = "landlock: " + err.Error()
		return r
	}
	if err := restrictSyscalls(); err != nil {
		r.Error = "seccomp: " + err.Error()
		return r
	}
	exe := filepath.Join(root, "runtime/probe")
	if err := unix.Exec(exe, []string{exe, checksFlag, root}, os.Environ()); err != nil {
		r.Error = "exec fixed probe: " + err.Error()
	}
	return r
}
func restrictFiles(root string) error {
	// ABI 3 baseline: all filesystem rights then available, including REFER/TRUNCATE.
	const handled = uint64((1 << 15) - 1)
	attr := unix.LandlockRulesetAttr{Handled_access_fs: handled}
	fd, _, e := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), 8, 0)
	if e != 0 {
		return e
	}
	defer unix.Close(int(fd))
	ro := uint64(unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR)
	rx := ro | unix.LANDLOCK_ACCESS_FS_EXECUTE
	rw := handled &^ (unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_MAKE_CHAR | unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK | unix.LANDLOCK_ACCESS_FS_MAKE_SOCK)
	rules := []struct {
		path   string
		access uint64
	}{{filepath.Join(root, "input"), ro}, {filepath.Join(root, "runtime"), rx}, {filepath.Join(root, "out"), rw}, {filepath.Join(root, "tmp"), rw}}
	for _, p := range runtimeRoots {
		if _, err := os.Stat(p); err == nil {
			rules = append(rules, struct {
				path   string
				access uint64
			}{p, rx})
		}
	}
	for _, rule := range rules {
		pathfd, err := unix.Open(rule.path, unix.O_PATH|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		// Kernel UAPI is packed (u64,u32), exactly 12 bytes; x/sys includes padding.
		a := unix.LandlockPathBeneathAttr{Allowed_access: rule.access, Parent_fd: int32(pathfd)}
		_, _, e = unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, fd, unix.LANDLOCK_RULE_PATH_BENEATH, uintptr(unsafe.Pointer(&a)), 0, 0, 0)
		unix.Close(pathfd)
		if e != 0 {
			return e
		}
	}
	_, _, e = unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, fd, 0, 0)
	if e != 0 {
		return e
	}
	return nil
}
func denySyscalls() []uint32 {
	return []uint32{
		unix.SYS_SOCKET, unix.SYS_SOCKETPAIR, unix.SYS_CONNECT, unix.SYS_BIND, unix.SYS_LISTEN, unix.SYS_ACCEPT, unix.SYS_ACCEPT4,
		unix.SYS_SENDTO, unix.SYS_SENDMSG, unix.SYS_SENDMMSG, unix.SYS_RECVFROM, unix.SYS_RECVMSG, unix.SYS_RECVMMSG,
		unix.SYS_IO_URING_SETUP, unix.SYS_IO_URING_ENTER, unix.SYS_IO_URING_REGISTER,
		unix.SYS_PTRACE, unix.SYS_PROCESS_VM_READV, unix.SYS_PROCESS_VM_WRITEV, unix.SYS_PIDFD_GETFD, unix.SYS_BPF,
		unix.SYS_MOUNT, unix.SYS_UMOUNT2, unix.SYS_PIVOT_ROOT, unix.SYS_OPEN_BY_HANDLE_AT, unix.SYS_SETNS, unix.SYS_UNSHARE,
	}
}
func restrictSyscalls() error {
	stmt := func(code uint16, k uint32) unix.SockFilter { return unix.SockFilter{Code: code, K: k} }
	jump := func(k uint32, jt, jf uint8) unix.SockFilter {
		return unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: k, Jt: jt, Jf: jf}
	}
	f := []unix.SockFilter{stmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, 4), jump(unix.AUDIT_ARCH_X86_64, 1, 0), stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_KILL_PROCESS), stmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, 0), {Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, K: 0x40000000, Jt: 0, Jf: 1}, stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ERRNO|uint32(unix.EPERM))}
	for _, nr := range denySyscalls() {
		f = append(f, jump(nr, 0, 1), stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ERRNO|uint32(unix.EPERM)))
	}
	f = append(f, stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ALLOW))
	prog := unix.SockFprog{Len: uint16(len(f)), Filter: &f[0]}
	v, _, e := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&prog)))
	runtime.KeepAlive(f)
	if e != 0 {
		return e
	}
	if v != 0 {
		return fmt.Errorf("TSYNC rejected thread %d", v)
	}
	return nil
}
func checks(root string) report {
	r := report{Backend: "landlock-seccomp", Status: "passed", Kernel: kernel(), RuntimeRoots: runtimeRoots}
	r.ABI, _ = landlockABI()
	r.Environment = hostEnvironment()
	add := func(name string, ok bool, err error) {
		c := check{Name: name, Passed: ok}
		if err != nil {
			c.Detail = err.Error()
		}
		r.Checks = append(r.Checks, c)
		if !ok {
			r.Status = "failed"
		}
	}
	denied := func(err error) bool { return errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM) }
	for _, p := range []string{"input/data", "runtime/data"} {
		data, err := os.ReadFile(filepath.Join(root, p))
		add("read-"+p, err == nil && string(data) == "fixture", err)
		err = os.WriteFile(filepath.Join(root, p), []byte("changed"), 0600)
		add("deny-write-"+p, denied(err), err)
	}
	for _, p := range []string{"outside", "/etc/passwd"} {
		path := p
		if !filepath.IsAbs(p) {
			path = filepath.Join(root, p)
		}
		_, err := os.ReadFile(path)
		add("deny-read-"+p, denied(err), err)
	}
	for _, d := range []string{"out", "tmp"} {
		p := filepath.Join(root, d, "created")
		err := os.WriteFile(p, []byte("output"), 0600)
		add("write-"+d, err == nil, err)
	}
	for _, family := range []int{unix.AF_UNIX, unix.AF_INET, unix.AF_INET6, unix.AF_NETLINK} {
		kind := unix.SOCK_STREAM
		if family == unix.AF_NETLINK {
			kind = unix.SOCK_RAW
		}
		fd, err := unix.Socket(family, kind, 0)
		if fd >= 0 {
			unix.Close(fd)
		}
		add(fmt.Sprintf("deny-socket-family-%d", family), errors.Is(err, unix.EPERM), err)
	}
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err == nil {
		unix.Close(fds[0])
		unix.Close(fds[1])
	}
	add("deny-socketpair", errors.Is(err, unix.EPERM), err)
	// Use invalid, side-effect-free arguments even when the filter is removed.
	// In particular, ptrace request 0 is PTRACE_TRACEME: a successful mutation
	// would change process tracing state and can hang its parent at the next exec.
	invalid := ^uintptr(0)
	for _, probe := range []struct {
		nr, arg1, arg2, arg3 uintptr
	}{
		{unix.SYS_IO_URING_SETUP, 0, 0, 0}, // zero entries and null params
		{unix.SYS_IO_URING_ENTER, invalid, 0, 0},
		{unix.SYS_IO_URING_REGISTER, invalid, invalid, 0},
		{unix.SYS_PTRACE, invalid, 0, 0}, // unknown request; never TRACEME/ATTACH
		{unix.SYS_BPF, invalid, 0, 0},
		{unix.SYS_PIDFD_GETFD, invalid, invalid, 0},
		{unix.SYS_SOCKET | 0x40000000, invalid, invalid, invalid},
	} {
		_, _, e := unix.Syscall6(probe.nr, probe.arg1, probe.arg2, probe.arg3, 0, 0, 0)
		add(fmt.Sprintf("deny-syscall-%d", probe.nr), e == unix.EPERM, e)
	}
	py := `import os,socket,sys
r=sys.argv[1]
assert open(r+'/input/data').read()=='fixture'
for p in [r+'/input/data',r+'/runtime/data']:
 try: open(p,'w'); raise AssertionError('write allowed: '+p)
 except PermissionError: pass
try: open(r+'/outside'); raise AssertionError('outside readable')
except PermissionError: pass
open(r+'/out/python-output','w').write('ok')
for f in [socket.AF_UNIX,socket.AF_INET,socket.AF_INET6,socket.AF_NETLINK]:
 try: socket.socket(f,socket.SOCK_STREAM); raise AssertionError('socket allowed')
 except PermissionError: pass
print('python exec inherited filesystem and socket restrictions')`
	cmd := exec.Command("/usr/bin/python3", "-B", "-c", py, root)
	cmd.Stdin = os.Stdin // reuse existing descriptor; do not grant ambient /dev/null
	out, err := cmd.CombinedOutput()
	add("python-exec-inheritance", err == nil, err)
	if err != nil {
		r.Error = strings.TrimSpace(string(out))
	}
	return r
}

func hostEnvironment() map[string]any {
	env := map[string]any{"uid": os.Getuid(), "euid": os.Geteuid(), "gid": os.Getgid(), "architecture": runtime.GOARCH}
	nnp, err := unix.PrctlRetInt(unix.PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0)
	if err == nil {
		env["no_new_privs"] = nnp
	}
	mode, err := unix.PrctlRetInt(unix.PR_GET_SECCOMP, 0, 0, 0, 0)
	if err == nil {
		env["seccomp_mode"] = mode
	}
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var caps [2]unix.CapUserData
	if unix.Capget(&hdr, &caps[0]) == nil {
		env["cap_effective_hex"] = strconv.FormatUint(uint64(caps[0].Effective)|uint64(caps[1].Effective)<<32, 16)
	}
	for name, path := range map[string]string{"limits": "/proc/self/limits", "uid_map": "/proc/self/uid_map", "unprivileged_userns_clone": "/proc/sys/kernel/unprivileged_userns_clone", "max_user_namespaces": "/proc/sys/user/max_user_namespaces"} {
		if data, err := os.ReadFile(path); err == nil {
			env[name] = strings.TrimSpace(string(data))
		}
	}
	return env
}
func hostControls(root string) []check {
	var controls []check
	add := func(name string, err error) {
		c := check{Name: name, Passed: err == nil}
		if err != nil {
			c.Detail = err.Error()
		}
		controls = append(controls, c)
	}
	for _, rel := range []string{"input/data", "runtime/data", "outside"} {
		path := filepath.Join(root, rel)
		data, err := os.ReadFile(path)
		add("host-read-"+rel, err)
		if err == nil {
			err = os.WriteFile(path, []byte("control-write"), 0600)
			add("host-write-"+rel, err)
			add("host-restore-"+rel, os.WriteFile(path, data, 0600))
		}
	}
	for _, family := range []int{unix.AF_UNIX, unix.AF_INET, unix.AF_INET6, unix.AF_NETLINK} {
		kind := unix.SOCK_STREAM
		if family == unix.AF_NETLINK {
			kind = unix.SOCK_RAW
		}
		fd, err := unix.Socket(family, kind, 0)
		if fd >= 0 {
			unix.Close(fd)
		}
		add(fmt.Sprintf("host-socket-family-%d", family), err)
	}
	return controls
}
