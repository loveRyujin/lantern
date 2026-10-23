//go:build linux

package sqlx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/loveRyujin/lantern/core/tools"
)

const limitArg = "--lantern-sql-limit-v1"
const workerArg = "--lantern-sql-worker-v1"

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
		if syscall.Exec(exe, []string{exe, workerArg}, os.Environ()) != nil {
			os.Exit(125)
		}
	case workerArg:
		os.Exit(sqlWorker(os.Stdin, os.Stdout))
	}
}

func sqlWorker(in io.Reader, out io.Writer) int {
	var lim syscall.Rlimit
	if syscall.Getrlimit(syscall.RLIMIT_AS, &lim) != nil || lim.Cur != ProcessMemoryBytes || lim.Max != ProcessMemoryBytes {
		return 125
	}
	stats := processStats{PID: os.Getpid(), MemoryLimit: lim.Cur}
	e := json.NewEncoder(out)
	e.SetEscapeHTML(false)
	if e.Encode(processMessage{Version: 1, Phase: "ready", Stats: stats}) != nil {
		return 125
	}
	d := json.NewDecoder(io.LimitReader(in, maxProcessInput))
	d.DisallowUnknownFields()
	var req processRequest
	if err := d.Decode(&req); err != nil {
		return 125
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return 125
	}
	res, code := executeProcessRequest(req)
	stats.PeakVirtual = procMemory("VmPeak:")
	stats.PeakRSS = procMemory("VmHWM:")
	m := processMessage{Version: 1, Phase: "done", Stats: stats, Code: code}
	if code == "" {
		m.Result = &res
	}
	if e.Encode(m) != nil {
		return 125
	}
	return 0
}

func executeProcessRequest(req processRequest) (Result, string) {
	if req.Limit < 1 || req.Limit > MaxLimit || Check(req.SQL) != nil {
		return Result{}, CodeRejected
	}
	if !sameProcessFile(req) {
		return Result{}, CodeBusy
	}
	ctx := context.Background()
	db, err := Open(ctx, req.Path, req.Allowed)
	if err != nil {
		if isBusy(err) {
			return Result{}, CodeBusy
		}
		return Result{}, CodeWorkerUnavailable
	}
	defer db.Close()
	if !sameProcessFile(req) {
		return Result{}, CodeBusy
	}
	before, err := Snapshot(req.Path)
	if err != nil {
		return Result{}, CodeBusy
	}
	res, qerr := Query(ctx, db, req.SQL, req.Limit)
	after, err := Snapshot(req.Path)
	if err != nil || before.Unchanged(after) != nil || !sameProcessFile(req) {
		return Result{}, CodeBusy
	}
	switch {
	case qerr == nil:
		return res, ""
	case rowTooLarge(qerr):
		return Result{}, CodeResultTooLarge
	case isBusy(qerr):
		return Result{}, CodeBusy
	case errors.Is(qerr, errTooManyColumns):
		return Result{}, "too_many_columns"
	default:
		return Result{}, CodeRejected
	}
}

func runSQLProcess(ctx context.Context, req processRequest) (Result, processStats, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, processStats{}, &codedTimeout{err: err}
	}
	// 直接调用此内部入口的测试也绑定文件身份；产品 Tool 已在构造时冻结身份。
	if req.Inode == 0 {
		info, err := os.Stat(req.Allowed)
		if err != nil {
			return Result{}, processStats{}, workerError(CodeWorkerUnavailable)
		}
		req.Device, req.Inode = fileIdentity(info)
	}
	exe, err := os.Executable()
	if err != nil {
		return Result{}, processStats{}, workerError(CodeWorkerUnavailable)
	}
	return runSQLProcessCommand(ctx, req, exec.CommandContext(ctx, exe, limitArg))
}

func fileIdentity(info os.FileInfo) (uint64, uint64) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(stat.Dev), stat.Ino
	}
	return 0, 0
}

func sameProcessFile(req processRequest) bool {
	resolved, err := Resolve(req.Path)
	if err != nil || resolved != req.Allowed || req.Path != req.Allowed {
		return false
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return false
	}
	dev, ino := fileIdentity(info)
	return ino != 0 && dev == req.Device && ino == req.Inode
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
	cmd.Env = []string{"GOMAXPROCS=2", "GOMEMLIMIT=64MiB", "TMPDIR=" + os.TempDir()}
	cmd.Stdin = bytes.NewReader(body)
	stdout := &cappedBuffer{limit: maxProcessOutput}
	stderr := &cappedBuffer{limit: 16 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
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
	if ctx.Err() != nil {
		return Result{}, stats, &codedTimeout{err: ctx.Err()}
	}
	ready, done, parseErr := parseProcessMessages(stdout.Bytes())
	if ready.Version == 1 && ready.Phase == "ready" && ready.Stats.PID == stats.PID && ready.Stats.MemoryLimit == ProcessMemoryBytes {
		stats.MemoryLimit = ready.Stats.MemoryLimit
	}
	if runErr != nil {
		// 只有已确认上限生效、并带明确运行时 OOM 诊断的异常退出才算内存超限。
		// SIGSEGV、普通 panic、SIGKILL 和协议损坏不会被伪装成 OOM。
		if stats.MemoryLimit == ProcessMemoryBytes && runtimeOOM(stderr.String()) {
			return Result{}, stats, resultTooLarge()
		}
		if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() == 125 {
			return Result{}, stats, workerError(CodeWorkerUnavailable)
		}
		return Result{}, stats, workerError(CodeWorkerFailed)
	}
	if parseErr != nil || stdout.overflow || stderr.overflow || done.Stats.PID != stats.PID {
		return Result{}, stats, workerError(CodeWorkerFailed)
	}
	stats.MemoryLimit = done.Stats.MemoryLimit
	if done.Stats.PeakVirtual > stats.PeakVirtual {
		stats.PeakVirtual = done.Stats.PeakVirtual
	}
	if done.Stats.PeakRSS > stats.PeakRSS {
		stats.PeakRSS = done.Stats.PeakRSS
	}
	switch done.Code {
	case "":
		if done.Result == nil {
			return Result{}, stats, workerError(CodeWorkerFailed)
		}
		encoded, err := encodeJSON(done.Result)
		if err != nil || len(encoded) > MaxResultBytes {
			return Result{}, stats, resultTooLarge()
		}
		return *done.Result, stats, nil
	case CodeResultTooLarge:
		return Result{}, stats, resultTooLarge()
	case CodeBusy:
		return Result{}, stats, busyErr()
	case CodeRejected:
		return Result{}, stats, rejected("查询无法执行，请检查表、列和表达式。")
	case "too_many_columns":
		return Result{}, stats, errTooManyColumns
	case CodeWorkerUnavailable:
		return Result{}, stats, workerError(CodeWorkerUnavailable)
	default:
		return Result{}, stats, workerError(CodeWorkerFailed)
	}
}

func runtimeOOM(stderr string) bool {
	return strings.Contains(stderr, "fatal error: out of memory") || strings.Contains(stderr, "runtime: out of memory:")
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

func workerError(code string) error {
	return &tools.Error{Code: code, Retryable: false, Message: "SQL 隔离进程不可用或异常退出，本次没有返回结果"}
}
