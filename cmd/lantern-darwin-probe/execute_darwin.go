//go:build darwin

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

type boundedBuffer struct {
	mu       sync.Mutex
	b        bytes.Buffer
	total    int64
	limit    int
	overflow chan struct{}
	once     sync.Once
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.total += int64(len(p))
	n := b.limit - b.b.Len()
	if n > len(p) {
		n = len(p)
	}
	if n > 0 {
		_, _ = b.b.Write(p[:n])
	}
	if b.total > int64(b.limit) {
		b.once.Do(func() { close(b.overflow) })
	}
	return len(p), nil
}
func (b *boundedBuffer) value() (string, int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String(), b.total
}

func defaultBudget() Budget {
	return Budget{CPUSeconds: 5, WallMS: 12000, RSSBytes: 512 << 20, OutputBytes: 64 << 10, DiskBytes: 32 << 20, CPURLimit: 8, FileRLimit: 16 << 20, NoFileRLimit: 128}
}
func fixedEnv(c taskConfig) []string {
	return []string{"PATH=/usr/bin:/bin", "LANG=en_US.UTF-8", "HOME=" + c.Tmp, "TMPDIR=" + c.Tmp, "MPLCONFIGDIR=" + filepath.Join(c.Tmp, "mpl"), "MPLBACKEND=Agg", "PYTHONDONTWRITEBYTECODE=1", "OPENBLAS_NUM_THREADS=1", "OMP_NUM_THREADS=1", "MKL_NUM_THREADS=1", "NUMEXPR_NUM_THREADS=1", "VECLIB_MAXIMUM_THREADS=1"}
}

func readTask(path string) (taskConfig, error) {
	var c taskConfig
	f, e := os.Open(path)
	if e != nil {
		return c, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, 64<<10+1))
	if e != nil {
		return c, e
	}
	if len(b) > 64<<10 {
		return c, fmt.Errorf("oversized fixed task config")
	}
	if e = json.Unmarshal(b, &c); e != nil {
		return c, e
	}
	if !fixedModes[c.Mode] || !filepath.IsAbs(c.Root) || !filepath.IsAbs(c.Python) {
		return c, fmt.Errorf("invalid fixed task")
	}
	b, e = os.ReadFile(c.Script)
	if e != nil || !bytes.Equal(b, pythonPayload) {
		return c, fmt.Errorf("fixed payload fingerprint mismatch")
	}
	if c.Budget.CPURLimit == 0 || c.Budget.CPURLimit > 10 || c.Budget.FileRLimit == 0 || c.Budget.FileRLimit > 32<<20 || c.Budget.NoFileRLimit == 0 || c.Budget.NoFileRLimit > 128 {
		return c, fmt.Errorf("invalid fixed experiment rlimit")
	}
	return c, nil
}

// The trusted shim applies inherited per-process rlimits before sandbox-exec.
// It installs no process/thread filter into the Go runtime. os/exec only passes
// stdio and explicit ready fd3; fd3 is CLOEXEC before the final exec.
func launch(path string) int {
	c, e := readTask(path)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		return 125
	}
	for _, limit := range []struct {
		which int
		value uint64
	}{{syscall.RLIMIT_CPU, c.Budget.CPURLimit}, {syscall.RLIMIT_FSIZE, c.Budget.FileRLimit}, {syscall.RLIMIT_NOFILE, c.Budget.NoFileRLimit}, {syscall.RLIMIT_CORE, 0}} {
		if e = syscall.Setrlimit(limit.which, &syscall.Rlimit{Cur: limit.value, Max: limit.value}); e != nil {
			fmt.Fprintln(os.Stderr, e)
			return 125
		}
	}
	if e = os.Chdir(c.Root); e != nil {
		return 125
	}
	exe, e := os.Executable()
	if e != nil {
		return 125
	}
	program := c.Python
	args := []string{program, "-I", "-B", c.Script, path}
	if c.Mode == "sql" {
		program = exe
		args = []string{exe, "__sql", path}
	}
	if c.Sandboxed {
		program = "/usr/bin/sandbox-exec"
		args = append([]string{program, "-f", c.Policy}, args...)
	}
	syscall.CloseOnExec(3)
	if f := os.NewFile(3, "exec-ready"); f != nil {
		_, _ = f.Write([]byte("ready\n"))
	}
	if e = syscall.Exec(program, args, fixedEnv(c)); e != nil {
		fmt.Fprintln(os.Stderr, "fixed exec:", e)
	}
	return 125
}

func internalMain(args []string) (bool, int) {
	if len(args) == 0 || !strings.HasPrefix(args[0], "__") {
		return false, 0
	}
	if len(args) != 2 {
		return true, 125
	}
	switch args[0] {
	case "__launch":
		return true, launch(args[1])
	case "__guardian":
		return true, guardian(args[1])
	case "__app":
		return true, app(args[1])
	case "__sql":
		return true, sqlTask(args[1])
	case "__signal-target":
		ch := make(chan os.Signal, 4)
		signal.Notify(ch, syscall.SIGUSR1)
		defer signal.Stop(ch)
		_ = os.WriteFile(args[1], []byte("ready"), 0600)
		deadline := time.NewTimer(3 * time.Minute)
		defer deadline.Stop()
		for {
			select {
			case <-ch:
				f, e := os.OpenFile(args[1], os.O_APPEND|os.O_WRONLY, 0600)
				if e == nil {
					_, _ = f.WriteString("\nsignal")
					_ = f.Close()
				}
			case <-deadline.C:
				return true, 0
			}
		}
	default:
		return true, 125
	}
}

func sqlTask(path string) int {
	c, e := readTask(path)
	if e != nil {
		return 125
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]interface{}{"entered": true, "mode": "sql", "pid": os.Getpid()})
	_ = writeJSON(filepath.Join(c.Out, "ready.json"), map[string]int{"pid": os.Getpid()})
	db, e := sql.Open("sqlite", filepath.Join(c.Out, "candidate.sqlite"))
	if e != nil {
		return 1
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var answer int64
	e = db.QueryRowContext(ctx, "WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<100000) SELECT sum(x) FROM n").Scan(&answer)
	if e != nil || answer != 5000050000 {
		fmt.Fprintln(os.Stderr, e)
		return 1
	}
	_ = json.NewEncoder(os.Stdout).Encode(Observation{Operation: "sqlite_sum", Attempted: true, Success: true, Detail: fmt.Sprint(answer)})
	return 0
}

func writeJSON(path string, v interface{}) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	return os.WriteFile(path, b, 0600)
}

func diskUsage(root string) (bytes int64, entries int, err error) {
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, e error) error {
		if os.IsNotExist(e) {
			return nil
		}
		if e != nil {
			return e
		}
		if path == root {
			return nil
		}
		entries++
		if d.Type().IsRegular() {
			st, e := d.Info()
			if os.IsNotExist(e) {
				return nil
			}
			if e != nil {
				return e
			}
			bytes += st.Size()
		}
		return nil
	})
	return
}

func runFixed(ctx context.Context, exe string, c taskConfig, monitor *processMonitor) Execution {
	r := Execution{Mode: c.Mode, Sandboxed: c.Sandboxed, ExitCode: -1, Budget: c.Budget, Usage: Usage{Scope: "single direct PID across trusted Go launcher, sandbox-exec, and fixed payload; CPU includes bootstrap; RSS and disk sampled, not hard quotas"}}
	conf := filepath.Join(c.Root, "input", "task.json")
	if e := writeJSON(conf, c); e != nil {
		r.Error = e.Error()
		return r
	}
	readyR, readyW, e := os.Pipe()
	if e != nil {
		r.Error = e.Error()
		return r
	}
	defer readyR.Close()
	cmd := exec.Command(exe, "__launch", conf)
	cmd.Dir = c.Root
	cmd.Env = fixedEnv(c)
	cmd.ExtraFiles = []*os.File{readyW}
	stdout := &boundedBuffer{limit: c.Budget.OutputBytes, overflow: make(chan struct{})}
	stderr := &boundedBuffer{limit: c.Budget.OutputBytes, overflow: make(chan struct{})}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// No new process group for profile discovery: setsid must not fail merely
	// because the harness made Python a process-group leader. All payloads are
	// fixed and bounded; only direct PID budgets are claimed in this spike.
	start := time.Now()
	if e = cmd.Start(); e != nil {
		readyW.Close()
		r.Error = e.Error()
		return r
	}
	readyW.Close()
	r.PID = cmd.Process.Pid
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	go func() { _, _ = io.Copy(io.Discard, readyR) }()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	wall := time.NewTimer(time.Duration(c.Budget.WallMS) * time.Millisecond)
	defer wall.Stop()
	var killed, lastSample time.Time
	stop := func(reason string) {
		if r.StopReason == "" {
			r.StopReason = reason
			killed = time.Now()
			_ = cmd.Process.Kill()
		}
	}
	finished := false
	for !finished {
		select {
		case e = <-done:
			finished = true
		case <-ctx.Done():
			stop("cancelled")
		case <-wall.C:
			stop("wall_limit")
		case <-stdout.overflow:
			stop("stdout_limit")
		case <-stderr.overflow:
			stop("stderr_limit")
		case <-ticker.C:
			now := time.Now()
			if !lastSample.IsZero() {
				gap := now.Sub(lastSample).Seconds() * 1000
				if gap > r.Usage.MaxSampleGapMS {
					r.Usage.MaxSampleGapMS = gap
				}
			}
			lastSample = now
			t, readErr := monitor.sample(r.PID)
			if readErr != nil { // May race final exit; Wait is the authoritative exit observation.
				select {
				case e = <-done:
					finished = true
				case <-time.After(20 * time.Millisecond):
					r.Usage.ReadErrors++
					stop("resource_read_failed")
				}
				continue
			}
			r.Usage.Samples++
			if t.Resident > r.Usage.PeakRSS {
				r.Usage.PeakRSS = t.Resident
			}
			if t.Virtual > r.Usage.PeakVirtual {
				r.Usage.PeakVirtual = t.Virtual
			}
			r.Usage.CPUSeconds = float64(t.User+t.System) / 1e9
			if t.Resident > c.Budget.RSSBytes {
				stop("rss_limit")
			}
			if r.Usage.CPUSeconds > c.Budget.CPUSeconds {
				stop("cpu_limit")
			}
			b, n, err := diskUsage(c.Out)
			bt, nt, et := diskUsage(c.Tmp)
			if err != nil || et != nil {
				stop("disk_read_failed")
			}
			b += bt
			n += nt
			if b > r.Usage.PeakDiskBytes {
				r.Usage.PeakDiskBytes = b
			}
			if n > r.Usage.PeakFiles {
				r.Usage.PeakFiles = n
			}
			if b > c.Budget.DiskBytes {
				stop("disk_limit")
			}
		}
	}
	r.Usage.WallSeconds = time.Since(start).Seconds()
	r.Usage.Waited = true
	if !killed.IsZero() {
		r.Usage.KillLatencyMS = time.Since(killed).Seconds() * 1000
	}
	if cmd.ProcessState != nil {
		r.ExitCode = cmd.ProcessState.ExitCode()
		if st, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && st.Signaled() {
			r.Signal = st.Signal().String()
		}
		cpu := cmd.ProcessState.UserTime().Seconds() + cmd.ProcessState.SystemTime().Seconds()
		if cpu > r.Usage.CPUSeconds {
			r.Usage.CPUSeconds = cpu
		}
	}
	r.Stdout, r.StdoutBytes = stdout.value()
	r.Stderr, r.StderrBytes = stderr.value()
	for _, line := range strings.Split(r.Stdout, "\n") {
		var v struct {
			Entered bool   `json:"entered"`
			PID     int    `json:"pid"`
			Mode    string `json:"mode"`
			Observation
		}
		if json.Unmarshal([]byte(line), &v) != nil {
			continue
		}
		if v.Entered && v.PID == r.PID && v.Mode == c.Mode {
			r.Entered = true
			r.PayloadPID = v.PID
		}
		if v.Operation != "" {
			r.Observations = append(r.Observations, v.Observation)
		}
	}
	if e != nil && r.StopReason == "" {
		r.Error = e.Error()
	}
	return r
}
