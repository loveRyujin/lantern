//go:build linux && amd64

package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

func installFiles(root string, plan runtimePlan) error {
	const all = uint64((1 << 15) - 1)
	attr := unix.LandlockRulesetAttr{Handled_access_fs: all}
	fd, _, e := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), 8, 0)
	if e != 0 {
		return e
	}
	defer unix.Close(int(fd))
	ro := uint64(unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR)
	rw := ro | unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_REMOVE_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_FILE | unix.LANDLOCK_ACCESS_FS_MAKE_DIR | unix.LANDLOCK_ACCESS_FS_MAKE_REG | unix.LANDLOCK_ACCESS_FS_MAKE_SYM | unix.LANDLOCK_ACCESS_FS_REFER | unix.LANDLOCK_ACCESS_FS_TRUNCATE
	type rule struct {
		p string
		a uint64
	}
	rules := []rule{{filepath.Join(root, "in"), ro}, {filepath.Join(root, "out"), rw}, {filepath.Join(root, "tmp"), rw}}
	for _, d := range plan.ReadDirs {
		rules = append(rules, rule{d, ro})
	}
	for _, f := range plan.ReadFiles {
		access := uint64(unix.LANDLOCK_ACCESS_FS_READ_FILE)
		for _, exe := range plan.ExecFiles {
			if exe == f {
				access |= unix.LANDLOCK_ACCESS_FS_EXECUTE
			}
		}
		rules = append(rules, rule{f, access})
	}
	exe, err := canonical(plan.Python)
	if err != nil {
		return err
	}
	rules = append(rules, rule{exe, uint64(unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_READ_FILE)})
	for _, r := range rules {
		f, err := unix.Open(r.p, unix.O_PATH|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		a := unix.LandlockPathBeneathAttr{Allowed_access: r.a, Parent_fd: int32(f)}
		_, _, e = unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, fd, unix.LANDLOCK_RULE_PATH_BENEATH, uintptr(unsafe.Pointer(&a)), 0, 0, 0)
		unix.Close(f)
		if e != 0 {
			return fmt.Errorf("Landlock %s: %w", r.p, e)
		}
	}
	_, _, e = unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, fd, 0, 0)
	if e != 0 {
		return e
	}
	return nil
}
func installFilter() error {
	deny := []uint32{
		unix.SYS_SOCKET, unix.SYS_SOCKETPAIR, unix.SYS_CONNECT, unix.SYS_BIND, unix.SYS_LISTEN, unix.SYS_ACCEPT, unix.SYS_ACCEPT4, unix.SYS_SENDTO, unix.SYS_SENDMSG, unix.SYS_SENDMMSG, unix.SYS_RECVFROM, unix.SYS_RECVMSG, unix.SYS_RECVMMSG,
		unix.SYS_IO_URING_SETUP, unix.SYS_IO_URING_ENTER, unix.SYS_IO_URING_REGISTER, unix.SYS_PTRACE, unix.SYS_PROCESS_VM_READV, unix.SYS_PROCESS_VM_WRITEV, unix.SYS_PIDFD_GETFD, unix.SYS_PIDFD_SEND_SIGNAL, unix.SYS_BPF,
		unix.SYS_PIDFD_OPEN, unix.SYS_PROCESS_MADVISE, unix.SYS_PROCESS_MRELEASE, unix.SYS_KCMP, unix.SYS_MIGRATE_PAGES, unix.SYS_MOVE_PAGES,
		unix.SYS_IO_SETUP, unix.SYS_IO_DESTROY, unix.SYS_IO_SUBMIT, unix.SYS_IO_CANCEL, unix.SYS_IO_GETEVENTS, unix.SYS_IO_PGETEVENTS,
		unix.SYS_MQ_OPEN, unix.SYS_MQ_UNLINK, unix.SYS_MQ_TIMEDSEND, unix.SYS_MQ_TIMEDRECEIVE, unix.SYS_MQ_NOTIFY, unix.SYS_MQ_GETSETATTR,
		unix.SYS_FALLOCATE, unix.SYS_MEMFD_CREATE, unix.SYS_MEMFD_SECRET, unix.SYS_SHMGET, unix.SYS_SHMAT, unix.SYS_SHMCTL, unix.SYS_SHMDT, unix.SYS_MSGGET, unix.SYS_MSGSND, unix.SYS_MSGRCV, unix.SYS_MSGCTL, unix.SYS_SEMGET, unix.SYS_SEMOP, unix.SYS_SEMCTL, unix.SYS_SEMTIMEDOP,
		unix.SYS_SETPRIORITY, unix.SYS_SCHED_SETAFFINITY, unix.SYS_SCHED_SETSCHEDULER, unix.SYS_SCHED_SETPARAM, unix.SYS_SCHED_SETATTR, unix.SYS_IOPRIO_SET,
		unix.SYS_SETUID, unix.SYS_SETGID, unix.SYS_SETREUID, unix.SYS_SETREGID, unix.SYS_SETRESUID, unix.SYS_SETRESGID, unix.SYS_SETFSUID, unix.SYS_SETFSGID, unix.SYS_SETGROUPS, unix.SYS_CAPSET,
		unix.SYS_UTIME, unix.SYS_UTIMES, unix.SYS_FUTIMESAT, unix.SYS_UTIMENSAT, unix.SYS_SETXATTR, unix.SYS_LSETXATTR, unix.SYS_FSETXATTR, unix.SYS_REMOVEXATTR, unix.SYS_LREMOVEXATTR, unix.SYS_FREMOVEXATTR,
		unix.SYS_KEYCTL, unix.SYS_ADD_KEY, unix.SYS_REQUEST_KEY, unix.SYS_USERFAULTFD, unix.SYS_PERF_EVENT_OPEN, unix.SYS_FANOTIFY_INIT, unix.SYS_FANOTIFY_MARK, unix.SYS_INOTIFY_INIT, unix.SYS_INOTIFY_INIT1, unix.SYS_INOTIFY_ADD_WATCH, unix.SYS_INOTIFY_RM_WATCH, unix.SYS_TIMER_CREATE,
		unix.SYS_FORK, unix.SYS_VFORK, unix.SYS_CLONE, unix.SYS_CLONE3, unix.SYS_SETSID, unix.SYS_SETPGID, unix.SYS_KILL, unix.SYS_TKILL, unix.SYS_TGKILL, unix.SYS_RT_SIGQUEUEINFO, unix.SYS_RT_TGSIGQUEUEINFO,
		unix.SYS_PRCTL, unix.SYS_FLOCK, unix.SYS_MOUNT, unix.SYS_UMOUNT2, unix.SYS_PIVOT_ROOT, unix.SYS_OPEN_BY_HANDLE_AT, unix.SYS_SETNS, unix.SYS_UNSHARE, unix.SYS_CHROOT,
		unix.SYS_CHMOD, unix.SYS_FCHMOD, unix.SYS_FCHMODAT, unix.SYS_FCHMODAT2, unix.SYS_CHOWN, unix.SYS_FCHOWN, unix.SYS_LCHOWN, unix.SYS_FCHOWNAT, unix.SYS_MKNOD, unix.SYS_MKNODAT,
	}
	stmt := func(code uint16, k uint32) unix.SockFilter { return unix.SockFilter{Code: code, K: k} }
	jeq := func(k uint32, jt, jf uint8) unix.SockFilter {
		return unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: k, Jt: jt, Jf: jf}
	}
	f := []unix.SockFilter{stmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, 4), jeq(unix.AUDIT_ARCH_X86_64, 1, 0), stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_KILL_PROCESS), stmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, 0), {Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, K: 0x40000000, Jf: 1}, stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ERRNO|uint32(unix.EPERM))}
	for _, nr := range deny {
		f = append(f, jeq(nr, 0, 1), stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ERRNO|uint32(unix.EPERM)))
	}
	// File-owner/async fcntl commands can send SIGIO to another same-UID
	// process without kill(2). Allow only ordinary FD/status/locking commands;
	// F_SETFL must never enable O_ASYNC. Only ordinary descriptor ioctls are permitted below.
	fcntlPolicy := []unix.SockFilter{stmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, 24)}
	for _, command := range []uint32{unix.F_DUPFD, unix.F_DUPFD_CLOEXEC, unix.F_GETFD, unix.F_SETFD, unix.F_GETFL, unix.F_GETLK, unix.F_GETPIPE_SZ} {
		fcntlPolicy = append(fcntlPolicy, jeq(command, 0, 1), stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ALLOW))
	}
	fcntlPolicy = append(fcntlPolicy,
		jeq(unix.F_SETFL, 0, 4),
		stmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, 32),
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, K: unix.O_ASYNC, Jf: 1},
		stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ERRNO|uint32(unix.EPERM)),
		stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ALLOW),
		stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ERRNO|uint32(unix.EPERM)))
	f = append(f, jeq(unix.SYS_FCNTL, 0, uint8(len(fcntlPolicy))))
	f = append(f, fcntlPolicy...)
	// CPython uses FIOCLEX when opening scripts. Preserve descriptor/query-only
	// commands without permitting FIOASYNC/FIOSETOWN or device-specific controls.
	ioctlPolicy := []unix.SockFilter{stmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, 24)}
	for _, command := range []uint32{0x5451 /* FIOCLEX */, 0x5450 /* FIONCLEX */, unix.TCGETS, unix.TIOCGWINSZ, unix.TIOCINQ, 0x5421 /* FIONBIO */} {
		ioctlPolicy = append(ioctlPolicy, jeq(command, 0, 1), stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ALLOW))
	}
	ioctlPolicy = append(ioctlPolicy, stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ERRNO|uint32(unix.EPERM)))
	f = append(f, jeq(unix.SYS_IOCTL, 0, uint8(len(ioctlPolicy))))
	f = append(f, ioctlPolicy...)
	// prlimit64 may inspect/change only self (pid 0), never the host guardian.
	f = append(f, jeq(unix.SYS_PRLIMIT64, 0, 4), stmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, 16), jeq(0, 1, 0), stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ERRNO|uint32(unix.EPERM)), stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ALLOW))
	f = append(f, stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ALLOW))
	p := unix.SockFprog{Len: uint16(len(f)), Filter: &f[0]}
	v, _, e := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&p)))
	runtime.KeepAlive(f)
	if e != 0 {
		return e
	}
	if v != 0 {
		return fmt.Errorf("seccomp TSYNC failed thread %d", v)
	}
	return nil
}
func pythonEnv(root string) []string {
	env := []string{"PATH=/nonexistent", "LANG=C.UTF-8", "PYTHONDONTWRITEBYTECODE=1", "PYTHONNOUSERSITE=1", "MPLBACKEND=Agg", "HOME=" + filepath.Join(root, "tmp"), "TMPDIR=" + filepath.Join(root, "tmp"), "MPLCONFIGDIR=" + filepath.Join(root, "tmp/mpl"), "INPUT_DIR=" + filepath.Join(root, "in"), "OUTPUT_DIR=" + filepath.Join(root, "out")}
	for _, k := range []string{"OPENBLAS_NUM_THREADS", "OMP_NUM_THREADS", "MKL_NUM_THREADS", "NUMEXPR_NUM_THREADS", "VECLIB_MAXIMUM_THREADS", "BLIS_NUM_THREADS"} {
		env = append(env, k+"=1")
	}
	return env
}

// A denylist is not a substitute for dropping ambient host privileges. This
// profile intentionally supports only ordinary non-root, capability-free users.
func checkUnprivileged() error {
	if os.Getuid() == 0 || os.Geteuid() == 0 {
		return fmt.Errorf("single-task profile requires a non-root user")
	}
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var caps [2]unix.CapUserData
	if err := unix.Capget(&hdr, &caps[0]); err != nil {
		return err
	}
	for _, cap := range caps {
		if cap.Effective != 0 || cap.Permitted != 0 {
			return fmt.Errorf("single-task profile requires zero effective/permitted capabilities")
		}
	}
	return nil
}
