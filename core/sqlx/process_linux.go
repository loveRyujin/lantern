//go:build linux

package sqlx

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// 自身重执行让测试、headless 和桌面端共享 worker。第二次 exec 之前设硬上限，
// Go 运行时也在上限内启动；不能先在已有的大地址空间里开查询，再补设限制。
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
		if syscall.Setrlimit(syscall.RLIMIT_AS, &syscall.Rlimit{Cur: ProcessMemoryBytes, Max: ProcessMemoryBytes}) != nil {
			os.Exit(125)
		}
		// 参数和 rlimit 都准备好之后再关 fd，紧挨着第二次 Exec。
		// Exec 本身不再打开描述符；失败则 125，不能带着未关闭的 fd 进入 worker。
		env := os.Environ()
		argv := []string{exe, workerArg}
		if closeInheritedDescriptors() != nil {
			os.Exit(125)
		}
		if syscall.Exec(exe, argv, env) != nil {
			os.Exit(125)
		}
	case workerArg:
		os.Exit(sqlWorker(os.Stdin, os.Stdout))
	}
}

// closeInheritedDescriptors 关掉除 stdin/stdout/stderr 以外的全部 fd。
//
// 不能依赖 FD_CLOEXEC。syscall.Exec 会保留每一个没有该标志的描述符；Go 在 Linux
// 上的 fork/exec 只把 stdio 安到 0、1、2，并不关闭 ≥3 的 fd。父进程用普通 open
// 打开、或事后清掉 CLOEXEC 的描述符，会穿过第一次 exec 留在这个 limit shim 里，
// 再被第二次 Exec 带进 worker，出现在 /proc/<pid>/fd。只在自己打开文件时加
// CLOEXEC 盖不住这些已经存在的描述符；临近 Exec 再逐个打标志也会漏掉间隙里新
// 开的 fd。close_range(3, ^uint(0), 0) 一次关掉整个范围，与标志无关。flags 必须
// 是 0：要关掉，而不是只标 CLOEXEC 或 unshare。任何错误（含内核不支持）都交给
// 调用方以 125 退出，不进入 worker。这条路径没有 /proc/self/fd 回退，避免扫不全
// 或扫描期间新开的 fd 被当成默认成功路径。
func closeInheritedDescriptors() error {
	var err error
	for range 16 {
		err = unix.CloseRange(3, ^uint(0), 0)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
	return err
}

func confirmedProcessMemory() (uint64, bool) {
	var lim syscall.Rlimit
	if syscall.Getrlimit(syscall.RLIMIT_AS, &lim) != nil || lim.Cur != ProcessMemoryBytes || lim.Max != ProcessMemoryBytes {
		return 0, false
	}
	return lim.Cur, true
}

func armWorkerGuards() error { return nil }

func workerMemorySample() (uint64, uint64) {
	return procMemory("VmPeak:"), procMemory("VmHWM:")
}

// runSQLProcessCommand 也是协议故障测试的命令注入点；产品入口只传固定的自身 shim。
func runSQLProcessCommand(ctx context.Context, req processRequest, cmd *exec.Cmd) (Result, processStats, error) {
	var stats processStats
	if err := ctx.Err(); err != nil {
		return Result{}, stats, &codedTimeout{err: err}
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
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.Cancel = func() error { return killSQLProcessGroup(cmd) }
	cmd.WaitDelay = time.Second
	// Linux Pdeathsig belongs to the creating OS thread, not the Go process.
	// Keep that thread alive through Wait, including failure/cancellation, so
	// trusted runtime thread retirement cannot kill an otherwise healthy worker.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	runErr := cmd.Start()
	if runErr == nil {
		stats.PID = cmd.Process.Pid
		stop := make(chan struct{})
		sampled := make(chan processStats, 1)
		go func() {
			var peak processStats
			ticker := time.NewTicker(2 * time.Millisecond)
			defer ticker.Stop()
			for {
				// shim 的 Go 运行时在设限前启动。只量第二次 exec 后的 worker，
				// 不把 shim 的虚拟地址预留算成受限查询的峰值。
				prefix := "/proc/" + strconv.Itoa(cmd.Process.Pid)
				args, _ := os.ReadFile(prefix + "/cmdline")
				var v, rss uint64
				if bytes.Contains(args, []byte(workerArg)) {
					v, rss = readProcessMemory(prefix + "/status")
				}
				if v > peak.PeakVirtual {
					peak.PeakVirtual = v
				}
				if rss > peak.PeakRSS {
					peak.PeakRSS = rss
				}
				select {
				case <-stop:
					sampled <- peak
					return
				case <-ticker.C:
				}
			}
		}()
		runErr = cmd.Wait() // Wait 也在取消后回收进程，不能留下僵尸。
		close(stop)
		peak := <-sampled
		stats.PeakVirtual, stats.PeakRSS = peak.PeakVirtual, peak.PeakRSS
	}
	if cmd.ProcessState != nil {
		stats.PID = cmd.ProcessState.Pid()
		if usage, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
			stats.PeakRSS = uint64(usage.Maxrss) * 1024
		}
	}
	return decodeWorkerOutcome(ctx, stdout, stderr, stats, cmd, runErr, false, false, false)
}

func procMemory(key string) uint64 {
	v, rss := readProcessMemory("/proc/self/status")
	if key == "VmPeak:" {
		return v
	}
	return rss
}

func readProcessMemory(path string) (virtual, rss uint64) {
	body, err := os.ReadFile(path)
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "VmPeak:") || strings.HasPrefix(line, "VmHWM:") {
			fields := strings.Fields(line)
			if len(fields) == 3 {
				n, _ := strconv.ParseUint(fields[1], 10, 64)
				if fields[0] == "VmPeak:" {
					virtual = n * 1024
				} else {
					rss = n * 1024
				}
			}
		}
	}
	return virtual, rss
}
