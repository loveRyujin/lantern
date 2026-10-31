//go:build linux || darwin

package sqlx

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/loveRyujin/lantern/core/tools"
)

const limitArg = "--lantern-sql-limit-v1"
const workerArg = "--lantern-sql-worker-v1"

// sqlWorker 是第二次 exec 之后的查询进程。内存上限必须已经按平台机制生效；
// 读回不等于 ProcessMemoryBytes 时直接 125，不能在未设限时执行查询。
func sqlWorker(in io.Reader, out io.Writer) int {
	limit, ok := confirmedProcessMemory()
	if !ok || limit != ProcessMemoryBytes {
		return 125
	}
	if err := armWorkerGuards(); err != nil {
		return 125
	}
	stats := processStats{PID: os.Getpid(), MemoryLimit: limit}
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
	stats.PeakVirtual, stats.PeakRSS = workerMemorySample()
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
	if err != nil {
		return false
	}
	allowed, err := Resolve(req.Allowed)
	// macOS 的临时目录在 /var → /private/var 后面。比较解析后的路径，
	// 同一文件的两种写法不会被当成替换。解析结果不同或 inode 变了仍是 data_busy。
	if err != nil || resolved != allowed {
		return false
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return false
	}
	dev, ino := fileIdentity(info)
	return ino != 0 && dev == req.Device && ino == req.Inode
}

// sqlWorkerEnv 是 worker 的全部环境。替换而不是追加，避免继承 API Key。
func sqlWorkerEnv() []string {
	return []string{"GOMAXPROCS=2", "GOMEMLIMIT=64MiB", "TMPDIR=" + os.TempDir()}
}

func killSQLProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

// decodeWorkerOutcome 把 worker 的退出分成超时、内存超限、启动失败和崩溃。
// memoryStop 只在平台已经观察到内存上限触发时为真；普通 SIGKILL 不会被当成超限。
// observationLost 表示已经看见 worker 之后无法继续读取它的内存，必须失败关闭。
func decodeWorkerOutcome(ctx context.Context, stdout, stderr *cappedBuffer, stats processStats, cmd *exec.Cmd, runErr error, workerSeen, memoryStop, observationLost bool) (Result, processStats, error) {
	if ctx.Err() != nil {
		return Result{}, stats, &codedTimeout{err: ctx.Err()}
	}
	ready, done, parseErr := parseProcessMessages(stdout.Bytes())
	if ready.Version == 1 && ready.Phase == "ready" && ready.Stats.PID == stats.PID && ready.Stats.MemoryLimit == ProcessMemoryBytes {
		stats.MemoryLimit = ready.Stats.MemoryLimit
	}
	confirmed := stats.MemoryLimit == ProcessMemoryBytes
	if runErr != nil {
		// 只有已确认上限生效、并带明确运行时 OOM 诊断的异常退出才算内存超限。
		// SIGSEGV、普通 panic、SIGKILL 和协议损坏不会被伪装成 OOM。
		if confirmed && runtimeOOM(stderr.String()) {
			return Result{}, stats, resultTooLarge()
		}
		if memoryStop && (workerSeen || confirmed) {
			return Result{}, stats, resultTooLarge()
		}
		if observationLost && workerSeen {
			return Result{}, stats, workerError(CodeWorkerUnavailable)
		}
		if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() == 125 {
			return Result{}, stats, workerError(CodeWorkerUnavailable)
		}
		return Result{}, stats, workerError(CodeWorkerFailed)
	}
	if memoryStop && (workerSeen || confirmed) {
		return Result{}, stats, resultTooLarge()
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

func workerError(code string) error {
	return &tools.Error{Code: code, Retryable: false, Message: "SQL 隔离进程不可用或异常退出，本次没有返回结果"}
}

// parseDarwinProcArgs 解析 KERN_PROCARGS2。布局是 argc、可执行文件、对齐用的 NUL、
// argc 个参数、再是环境变量。Darwin 的环境测试用它确认 worker 没有继承 API Key。
func parseDarwinProcArgs(buf []byte) (argv, env []string, err error) {
	if len(buf) < 4 {
		return nil, nil, errors.New("sqlx: procargs 太短")
	}
	argc := int(binary.NativeEndian.Uint32(buf[:4]))
	rest := buf[4:]
	nul := bytes.IndexByte(rest, 0)
	if nul < 0 {
		return nil, nil, errors.New("sqlx: procargs 没有可执行文件")
	}
	rest = rest[nul+1:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	if argc < 0 || argc > 4096 {
		return nil, nil, errors.New("sqlx: procargs argc 无效")
	}
	argv = make([]string, 0, argc)
	for len(argv) < argc {
		nul = bytes.IndexByte(rest, 0)
		if nul < 0 {
			return nil, nil, errors.New("sqlx: procargs argv 被截断")
		}
		argv = append(argv, string(rest[:nul]))
		rest = rest[nul+1:]
	}
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	for len(rest) > 0 {
		nul = bytes.IndexByte(rest, 0)
		if nul <= 0 {
			break
		}
		env = append(env, string(rest[:nul]))
		rest = rest[nul+1:]
	}
	return argv, env, nil
}
