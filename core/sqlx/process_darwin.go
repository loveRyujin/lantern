//go:build darwin

package sqlx

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/unix"
)

// Darwin 没有能在普通进程里生效的 RLIMIT_AS。
//
// x/sys 里 Darwin 的 RLIMIT_AS 和 RLIMIT_RSS 都是 5。setrlimit 对这个资源返回
// EINVAL；即便调用成功，手册也写明内核不强制，分配可以越过它。把成功返回值当成
// 硬上限会静默失效。memorystatus_control 在进入具体命令之前就要求 root 或
// com.apple.private.memorystatus；GitHub 的 macOS runner 没有这份 entitlement，
// 调用得到 EPERM。task_set_phys_footprint_limit 是 jetsam 协调接口，普通进程上
// 测到的是错误返回，而且随后的分配仍然成功。这两条都不装进 worker：装不上就
// 退出 125 会让每条查询都变成 sql_worker_unavailable；忽略错误继续跑则会假装
// 已经有内核配额。
//
// 实际停止线是 proc_pid_rusage(RUSAGE_INFO_V2) 的 ri_phys_footprint，单位是字节，
// 阈值就是 ProcessMemoryBytes。它是 Activity Monitor 的 Memory，含压缩和换出的
// 匿名页，不是虚拟地址空间，也不是 proc_pidinfo 的驻留页。worker 在发 ready 之前
// 必须能读到自己的非零足迹，并每 2ms 再读一次；父进程只在看见 worker 映像之后
// 做同样的采样。任一侧发现足迹大于 1 GiB 就停进程。这是采样停止，允许大约一个
// 采样间隔的超调，不是内核在缺页时返回 ENOMEM。
//
// 已经看见 worker 之后，足迹连续约 50 次读不到，就杀掉进程并返回
// sql_worker_unavailable。不在看不见内存时继续查询，也不退回父进程。
const (
	rusageInfoV2       = 2
	kernProcArgs2      = 49
	procPIDTaskInfo    = 4
	darwinBlindSamples = 50
	darwinSampleEvery  = 2 * time.Millisecond
)

// darwinRusageV2 与 struct rusage_info_v2 的布局一致。ri_phys_footprint 是字节。
type darwinRusageV2 struct {
	UUID                                                                          [16]byte
	UserTime, SystemTime, PkgIdle, Interrupt, Pageins, Wired, Resident, Footprint uint64
	Start, Exit                                                                   uint64
	ChildUser, ChildSystem, ChildPkg, ChildInterrupt, ChildPageins, ChildElapsed  uint64
	DiskRead, DiskWrite                                                           uint64
}

// darwinTaskInfo 与 proc_taskinfo 一致，只为日志里的虚拟地址大小。停止线不看它。
type darwinTaskInfo struct {
	Virtual, Resident, User, System, LiveUser, LiveSystem                                                                                uint64
	Policy, Faults, Pageins, COWFaults, MessagesSent, MessagesReceived, SyscallsMach, SyscallsUnix, Switches, Threads, Running, Priority int32
}

type darwinSample struct {
	peak            processStats
	workerSeen      bool
	memoryStop      bool
	observationLost bool
}

var (
	libcOnce    sync.Once
	libcErr     error
	libcErrno   func() uintptr
	libcSysctl  func(name *int32, namelen uint32, oldp unsafe.Pointer, oldlenp *uintptr, newp unsafe.Pointer, newlen uintptr) int32
	libcPidInfo func(pid int32, flavor int32, arg uint64, buffer unsafe.Pointer, buffersize int32) int32
	libcRusage  func(pid int32, flavor int32, buffer unsafe.Pointer) int32
)

func init() {
	if len(os.Args) != 2 {
		return
	}
	switch os.Args[1] {
	case limitArg:
		exe, err := os.Executable()
		if err != nil {
			os.Exit(125)
		}
		if syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{}) != nil {
			os.Exit(125)
		}
		// 不设置 RLIMIT_AS，也不调用 memorystatus_control。两者都不是这条路径上
		// 能够核实的硬上限。足迹停止在第二次 exec 之后的 worker 里武装。
		env := os.Environ()
		if syscall.Exec(exe, []string{exe, workerArg}, env) != nil {
			os.Exit(125)
		}
	case workerArg:
		os.Exit(sqlWorker(os.Stdin, os.Stdout))
	}
}

func loadDarwinLibc() error {
	libcOnce.Do(func() {
		system, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW|purego.RTLD_LOCAL)
		if err != nil {
			libcErr = err
			return
		}
		proc, err := purego.Dlopen("/usr/lib/libproc.dylib", purego.RTLD_NOW|purego.RTLD_LOCAL)
		if err != nil {
			libcErr = err
			return
		}
		errSym, err := purego.Dlsym(system, "__error")
		if err != nil {
			libcErr = err
			return
		}
		sysSym, err := purego.Dlsym(system, "sysctl")
		if err != nil {
			libcErr = err
			return
		}
		infoSym, err := purego.Dlsym(proc, "proc_pidinfo")
		if err != nil {
			libcErr = err
			return
		}
		rusageSym, err := purego.Dlsym(proc, "proc_pid_rusage")
		if err != nil {
			libcErr = err
			return
		}
		purego.RegisterFunc(&libcErrno, errSym)
		purego.RegisterFunc(&libcSysctl, sysSym)
		purego.RegisterFunc(&libcPidInfo, infoSym)
		purego.RegisterFunc(&libcRusage, rusageSym)
	})
	return libcErr
}

func darwinErrno(rc int32) error {
	if rc == 0 {
		return nil
	}
	if libcErrno == nil {
		return errors.New("sqlx: darwin libc 调用失败")
	}
	p := libcErrno()
	if p == 0 {
		return errors.New("sqlx: darwin libc 调用失败")
	}
	return syscall.Errno(*(*int32)(unsafe.Pointer(p)))
}

func confirmedProcessMemory() (uint64, bool) {
	fp, err := darwinFootprint(os.Getpid())
	// 发 ready 之前必须证明足迹读得出来，而且进程还没越过同一字节数。
	// 读不到就 125，不在未设停止线时执行查询。
	if err != nil || fp == 0 || fp > ProcessMemoryBytes {
		return 0, false
	}
	return ProcessMemoryBytes, true
}

// armWorkerGuards 武装两件 worker 自己的事：父进程死亡，以及足迹停止。
// Darwin 没有 Pdeathsig。kqueue 在第二次 exec 之后才武装，shim 到 exec 之间
// 有一段没有父死信号的窗口。注册失败或足迹读不出来则 125。
func armWorkerGuards() error {
	ppid := os.Getppid()
	if ppid <= 1 {
		return errors.New("sqlx: worker 已失去父进程")
	}
	kq, err := unix.Kqueue()
	if err != nil {
		return err
	}
	unix.CloseOnExec(kq)
	change := unix.Kevent_t{
		Ident:  uint64(ppid),
		Filter: unix.EVFILT_PROC,
		Flags:  unix.EV_ADD | unix.EV_ENABLE,
		Fflags: unix.NOTE_EXIT,
	}
	if _, err := unix.Kevent(kq, []unix.Kevent_t{change}, nil, nil); err != nil {
		_ = unix.Close(kq)
		return err
	}
	go func() {
		defer unix.Close(kq)
		var ev [1]unix.Kevent_t
		_, _ = unix.Kevent(kq, nil, ev[:], nil)
		_ = syscall.Kill(0, syscall.SIGKILL)
	}()
	return armDarwinFootprintStop()
}

func armDarwinFootprintStop() error {
	fp, err := darwinFootprint(os.Getpid())
	if err != nil || fp == 0 || fp > ProcessMemoryBytes {
		if err == nil {
			err = errors.New("sqlx: 进程足迹已经超过上限")
		}
		return err
	}
	go func() {
		var failures int
		ticker := time.NewTicker(darwinSampleEvery)
		defer ticker.Stop()
		for range ticker.C {
			got, readErr := darwinFootprint(os.Getpid())
			if readErr != nil || got == 0 {
				failures++
				if failures >= darwinBlindSamples {
					os.Exit(125)
				}
				continue
			}
			failures = 0
			if got > ProcessMemoryBytes {
				_, _ = os.Stderr.WriteString("fatal error: out of memory\n")
				os.Exit(2)
			}
		}
	}()
	return nil
}

func workerMemorySample() (uint64, uint64) {
	var virtual, rss uint64
	if v, _, err := darwinTaskMemory(os.Getpid()); err == nil {
		virtual = v
	}
	if fp, err := darwinFootprint(os.Getpid()); err == nil && fp > rss {
		rss = fp
	}
	var ru syscall.Rusage
	// getrusage 的 ru_maxrss 在 Darwin 上是字节，而且是驻留峰值，不是 phys_footprint。
	// 只在它更大时并进报告，不能乘 1024，也不能拿它代替停止线。
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) == nil && ru.Maxrss > 0 && uint64(ru.Maxrss) > rss {
		rss = uint64(ru.Maxrss)
	}
	return virtual, rss
}

func runSQLProcessCommand(ctx context.Context, req processRequest, cmd *exec.Cmd) (Result, processStats, error) {
	var stats processStats
	if err := ctx.Err(); err != nil {
		return Result{}, stats, &codedTimeout{err: err}
	}
	if err := loadDarwinLibc(); err != nil {
		return Result{}, stats, workerError(CodeWorkerUnavailable)
	}
	body, err := encodeJSON(req)
	if err != nil || len(body) > maxProcessInput {
		return Result{}, stats, workerError(CodeWorkerUnavailable)
	}
	cmd.Env = sqlWorkerEnv()
	cmd.Stdin = bytes.NewReader(body)
	stdout := &cappedBuffer{limit: maxProcessOutput}
	stderr := &cappedBuffer{limit: 16 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// 新进程组让取消和足迹停止能杀掉 shim/worker。没有 Pdeathsig。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killSQLProcessGroup(cmd) }
	cmd.WaitDelay = time.Second
	runErr := cmd.Start()
	var seen darwinSample
	kq := -1
	if runErr == nil {
		stats.PID = cmd.Process.Pid
		if fd, qerr := darwinExitQueue(cmd.Process.Pid); qerr == nil {
			kq = fd
			defer unix.Close(kq)
		}
		stop := make(chan struct{})
		sampled := make(chan darwinSample, 1)
		go watchDarwinWorker(cmd.Process.Pid, stop, sampled)
		runErr = cmd.Wait()
		close(stop)
		seen = <-sampled
		stats.PeakVirtual = seen.peak.PeakVirtual
		stats.PeakRSS = seen.peak.PeakRSS
		if darwinExitWasMemory(kq) {
			seen.memoryStop = true
		}
	}
	if cmd.ProcessState != nil {
		stats.PID = cmd.ProcessState.Pid()
		if usage, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok && usage.Maxrss > 0 {
			rss := uint64(usage.Maxrss)
			if rss > stats.PeakRSS {
				stats.PeakRSS = rss
			}
		}
	}
	return decodeWorkerOutcome(ctx, stdout, stderr, stats, cmd, runErr, seen.workerSeen, seen.memoryStop, seen.observationLost)
}

func watchDarwinWorker(pid int, stop <-chan struct{}, out chan<- darwinSample) {
	var sample darwinSample
	var failures int
	ticker := time.NewTicker(darwinSampleEvery)
	defer ticker.Stop()
	for {
		if !sample.workerSeen {
			argv, _, err := readDarwinProcessArgs(pid)
			if err == nil && bytes.Contains([]byte(strings.Join(argv, "\x00")), []byte(workerArg)) {
				sample.workerSeen = true
				failures = 0
			}
		}
		if sample.workerSeen {
			fp, err := darwinFootprint(pid)
			switch {
			case err == nil && fp > 0:
				failures = 0
				if fp > sample.peak.PeakRSS {
					sample.peak.PeakRSS = fp
				}
				if virtual, _, verr := darwinTaskMemory(pid); verr == nil && virtual > sample.peak.PeakVirtual {
					sample.peak.PeakVirtual = virtual
				}
				if fp > ProcessMemoryBytes {
					sample.memoryStop = true
					_ = syscall.Kill(-pid, syscall.SIGKILL)
					out <- sample
					return
				}
			case errors.Is(err, syscall.ESRCH):
				failures = 0
			default:
				if kerr := syscall.Kill(pid, 0); !errors.Is(kerr, syscall.ESRCH) {
					failures++
					if failures >= darwinBlindSamples {
						sample.observationLost = true
						_ = syscall.Kill(-pid, syscall.SIGKILL)
						out <- sample
						return
					}
				}
			}
		}
		select {
		case <-stop:
			out <- sample
			return
		case <-ticker.C:
		}
	}
}

func darwinFootprint(pid int) (uint64, error) {
	if err := loadDarwinLibc(); err != nil {
		return 0, err
	}
	if pid <= 0 || libcRusage == nil {
		return 0, syscall.ESRCH
	}
	var info darwinRusageV2
	if rc := libcRusage(int32(pid), rusageInfoV2, unsafe.Pointer(&info)); rc != 0 {
		return 0, darwinErrno(rc)
	}
	return info.Footprint, nil
}

func darwinTaskMemory(pid int) (virtual, rss uint64, err error) {
	if err := loadDarwinLibc(); err != nil {
		return 0, 0, err
	}
	if pid <= 0 {
		return 0, 0, syscall.ESRCH
	}
	var info darwinTaskInfo
	n := libcPidInfo(int32(pid), procPIDTaskInfo, 0, unsafe.Pointer(&info), int32(unsafe.Sizeof(info)))
	if n <= 0 {
		return 0, 0, darwinErrno(-1)
	}
	if n != int32(unsafe.Sizeof(info)) {
		return 0, 0, errors.New("sqlx: proc_pidinfo 长度不对")
	}
	return info.Virtual, info.Resident, nil
}

func darwinExitQueue(pid int) (int, error) {
	kq, err := unix.Kqueue()
	if err != nil {
		return -1, err
	}
	unix.CloseOnExec(kq)
	change := unix.Kevent_t{
		Ident:  uint64(pid),
		Filter: unix.EVFILT_PROC,
		Flags:  unix.EV_ADD | unix.EV_ENABLE,
		Fflags: unix.NOTE_EXIT | unix.NOTE_EXIT_DETAIL,
	}
	if _, err := unix.Kevent(kq, []unix.Kevent_t{change}, nil, nil); err != nil {
		_ = unix.Close(kq)
		return -1, err
	}
	return kq, nil
}

func darwinExitWasMemory(kq int) bool {
	if kq < 0 {
		return false
	}
	ts := unix.NsecToTimespec(0)
	var ev [8]unix.Kevent_t
	n, err := unix.Kevent(kq, nil, ev[:], &ts)
	if err != nil || n <= 0 {
		return false
	}
	for i := range n {
		if ev[i].Flags&unix.EV_ERROR != 0 {
			continue
		}
		if ev[i].Filter == unix.EVFILT_PROC && ev[i].Fflags&unix.NOTE_EXIT_MEMORY != 0 {
			return true
		}
	}
	return false
}

// readDarwinProcessArgs 读取 KERN_PROCARGS2。测试用它确认 worker 环境没有 API Key。
func readDarwinProcessArgs(pid int) (argv, env []string, err error) {
	if pid <= 0 {
		return nil, nil, syscall.ESRCH
	}
	if err := loadDarwinLibc(); err != nil {
		return nil, nil, err
	}
	mib := [3]int32{unix.CTL_KERN, kernProcArgs2, int32(pid)}
	var need uintptr
	if rc := libcSysctl(&mib[0], uint32(len(mib)), nil, &need, nil, 0); rc != 0 {
		err := darwinErrno(rc)
		if !errors.Is(err, syscall.ENOMEM) || need == 0 || need > 1<<20 {
			return nil, nil, err
		}
	}
	if need == 0 || need > 1<<20 {
		return nil, nil, errors.New("sqlx: procargs 长度无效")
	}
	buf := make([]byte, need)
	got := need
	if rc := libcSysctl(&mib[0], uint32(len(mib)), unsafe.Pointer(&buf[0]), &got, nil, 0); rc != 0 {
		return nil, nil, darwinErrno(rc)
	}
	if got == 0 || got > uintptr(len(buf)) {
		return nil, nil, errors.New("sqlx: procargs 长度无效")
	}
	return parseDarwinProcArgs(buf[:got])
}
