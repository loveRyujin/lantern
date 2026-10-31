//go:build darwin

package sqlx

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/loveRyujin/lantern/core/tools"
)

type parentMustNotQuery struct{ t *testing.T }

func (s parentMustNotQuery) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	s.t.Fatal("SQL was executed in the parent process")
	return nil, errors.New("parent query forbidden")
}

func darwinProcessPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "analytics.db")
	createEmptyDB(t, path)
	// macOS 的临时目录在 /var → /private/var 后面。worker 要求路径和 EvalSymlinks 的结果一致，
	// 否则会把同一文件当成被替换，返回 data_busy。
	resolved, err := Resolve(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func assertDarwinReaped(t *testing.T, stats processStats) {
	t.Helper()
	if stats.PID <= 0 || stats.PID == os.Getpid() {
		t.Fatalf("invalid worker PID: %+v", stats)
	}
	if err := syscall.Kill(stats.PID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("worker %d was not reaped: %v", stats.PID, err)
	}
}

func assertDarwinCode(t *testing.T, err error, want string) {
	t.Helper()
	var te *tools.Error
	if !errors.As(err, &te) || te.Code != want || te.Retryable {
		t.Fatalf("error = %v; want nonretryable %s", err, want)
	}
}

func TestDarwinRusageV2Layout(t *testing.T) {
	var info darwinRusageV2
	if unsafe.Sizeof(info) != 160 {
		t.Fatalf("rusage_info_v2 size = %d, want 160", unsafe.Sizeof(info))
	}
	if unsafe.Offsetof(info.Footprint) != 16+7*8 {
		t.Fatalf("phys_footprint offset = %d", unsafe.Offsetof(info.Footprint))
	}
}

func TestDarwinSQLProcessReadOnlyQuery(t *testing.T) {
	path := darwinProcessPath(t)
	tool, err := NewTool(parentMustNotQuery{t}, path, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := tool.Execute(t.Context(), sqlCall(t, "SELECT 42 AS n"))
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Rows [][]int `json:"rows"`
	}
	if err := json.Unmarshal([]byte(result.Content), &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Rows) != 1 || len(parsed.Rows[0]) != 1 || parsed.Rows[0][0] != 42 {
		t.Fatalf("result = %s", result.Content)
	}
	_, stats, err := runSQLProcess(t.Context(), processRequest{Path: path, Allowed: path, SQL: "SELECT 1", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if stats.MemoryLimit != ProcessMemoryBytes || stats.PeakRSS == 0 || stats.PeakRSS > 512<<20 {
		t.Fatalf("worker memory limit was not the 1 GiB contract: %+v", stats)
	}
	// 虚拟地址可以越过 1 GiB。Darwin 的上限不是 RLIMIT_AS。
	t.Logf("resident=%d virtual=%d limit=%d", stats.PeakRSS, stats.PeakVirtual, stats.MemoryLimit)
	assertDarwinReaped(t, stats)
}

func TestDarwinSQLProcessRejectsWriteAndAttach(t *testing.T) {
	path := darwinProcessPath(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"INSERT INTO t VALUES (1)",
		"UPDATE t SET id = 2",
		"ATTACH DATABASE ':memory:' AS s",
		"ATTACH DATABASE 'file:other.db?mode=rwc' AS s",
	} {
		t.Run(query, func(t *testing.T) {
			res, stats, err := runSQLProcess(t.Context(), processRequest{Path: path, Allowed: path, SQL: query, Limit: 1})
			assertDarwinCode(t, err, CodeRejected)
			if res.RowCount != 0 || len(res.Rows) != 0 {
				t.Fatalf("rejected query returned rows: %+v", res)
			}
			assertDarwinReaped(t, stats)
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("rejected query changed the database")
			}
		})
	}
}

func TestDarwinSQLProcessRegisteredFileReplacement(t *testing.T) {
	for _, kind := range []string{"symlink", "rename"} {
		t.Run(kind, func(t *testing.T) {
			path := darwinProcessPath(t)
			other := filepath.Join(filepath.Dir(path), "replacement.db")
			createEmptyDB(t, other)
			tool, err := NewTool(parentMustNotQuery{t}, path, path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(path, path+".original"); err != nil {
				t.Fatal(err)
			}
			if kind == "symlink" {
				err = os.Symlink(other, path)
			} else {
				err = os.Rename(other, path)
			}
			if err != nil {
				t.Fatal(err)
			}
			res, err := tool.Execute(t.Context(), sqlCall(t, "SELECT 1"))
			var te *tools.Error
			if !errors.As(err, &te) || te.Code != CodeBusy || !te.Retryable || res.Content != "" {
				t.Fatalf("registered file changed: result=%+v error=%v", res, err)
			}
		})
	}
}

func TestDarwinSQLProcessTimeoutReapsAndRecovers(t *testing.T) {
	path := darwinProcessPath(t)
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	query := "WITH RECURSIVE x(n) AS (VALUES(0) UNION ALL SELECT n+1 FROM x WHERE n<1000000000) SELECT sum(n) FROM x"
	start := time.Now()
	_, stats, err := runSQLProcess(ctx, processRequest{Path: path, Allowed: path, SQL: query, Limit: 1})
	var timeout *codedTimeout
	if !errors.As(err, &timeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v; want deadline exceeded", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("worker cancellation did not return promptly")
	}
	assertDarwinReaped(t, stats)
	res, next, err := runSQLProcess(t.Context(), processRequest{Path: path, Allowed: path, SQL: "SELECT 42", Limit: 1})
	if err != nil || res.RowCount != 1 {
		t.Fatalf("query after cancellation: result=%+v error=%v", res, err)
	}
	if next.MemoryLimit != ProcessMemoryBytes {
		t.Fatalf("recovered worker lost its memory limit: %+v", next)
	}
	assertDarwinReaped(t, next)
}

func TestDarwinSQLProcessEnvironmentDoesNotInheritKeys(t *testing.T) {
	const sentinel = "sk-darwin-sql-must-not-inherit"
	t.Setenv("OPENAI_API_KEY", sentinel)
	t.Setenv("ANTHROPIC_API_KEY", sentinel)
	t.Setenv("LANTERN_API_KEY", sentinel)
	path := darwinProcessPath(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	query := "WITH RECURSIVE x(n) AS (VALUES(0) UNION ALL SELECT n+1 FROM x WHERE n<1000000000) SELECT sum(n) FROM x"
	done := make(chan error, 1)
	go func() {
		_, _, err := runSQLProcess(ctx, processRequest{Path: path, Allowed: path, SQL: query, Limit: 1})
		done <- err
	}()
	deadline := time.Now().Add(8 * time.Second)
	var env []string
	for time.Now().Before(deadline) {
		for _, pid := range darwinWorkerPIDs() {
			argv, got, err := readDarwinProcessArgs(pid)
			if err != nil || !strings.Contains(strings.Join(argv, "\x00"), workerArg) {
				continue
			}
			env = got
			break
		}
		if env != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	err := <-done
	var timeout *codedTimeout
	if !errors.As(err, &timeout) {
		t.Fatalf("slow query did not cancel: %v", err)
	}
	if env == nil {
		t.Fatal("did not observe the worker environment")
	}
	allowed := map[string]bool{"GOMAXPROCS=2": true, "GOMEMLIMIT=64MiB": true}
	sawTmp := false
	for _, item := range env {
		if strings.Contains(item, sentinel) || strings.Contains(item, "API_KEY") {
			t.Fatalf("worker inherited a secret: %s", item)
		}
		if strings.HasPrefix(item, "TMPDIR=") && len(item) > len("TMPDIR=") {
			sawTmp = true
			continue
		}
		if !allowed[item] {
			t.Fatalf("worker environment is not the allowlist: %q in %v", item, env)
		}
	}
	if !sawTmp || !allowedPresent(env, "GOMAXPROCS=2") || !allowedPresent(env, "GOMEMLIMIT=64MiB") {
		t.Fatalf("worker environment = %v", env)
	}
}

func allowedPresent(env []string, want string) bool {
	for _, item := range env {
		if item == want {
			return true
		}
	}
	return false
}

func TestDarwinSQLProcessOverLimitFailClosed(t *testing.T) {
	path := darwinProcessPath(t)
	const columns = 320
	parts := make([]string, columns)
	for i := range parts {
		parts[i] = fmt.Sprintf("hex(zeroblob(2000000)) AS c%d", i)
	}
	query := "WITH x AS MATERIALIZED (SELECT " + strings.Join(parts, ", ") + ") SELECT count(*) FROM x"
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	res, stats, err := runSQLProcess(ctx, processRequest{Path: path, Allowed: path, SQL: query, Limit: 1})
	t.Logf("pid=%d rss=%d virtual=%d limit=%d error=%v", stats.PID, stats.PeakRSS, stats.PeakVirtual, stats.MemoryLimit, err)
	assertDarwinCode(t, err, CodeResultTooLarge)
	if res.RowCount != 0 || len(res.Rows) != 0 {
		t.Fatalf("over-limit worker returned rows: %+v", res)
	}
	assertDarwinReaped(t, stats)
	follow, next, err := runSQLProcess(t.Context(), processRequest{Path: path, Allowed: path, SQL: "SELECT 7", Limit: 1})
	if err != nil || follow.RowCount != 1 {
		t.Fatalf("query after over-limit: result=%+v error=%v", follow, err)
	}
	if next.MemoryLimit != ProcessMemoryBytes {
		t.Fatalf("next worker did not confirm the same cap: %+v", next)
	}
}

func TestDarwinSQLProcessParentDeathReaps(t *testing.T) {
	if os.Getenv("LANTERN_SQL_PARENT_DEATH") == "1" {
		path := os.Getenv("LANTERN_SQL_DB")
		query := "WITH RECURSIVE x(n) AS (VALUES(0) UNION ALL SELECT n+1 FROM x WHERE n<1000000000) SELECT sum(n) FROM x"
		_, _, _ = runSQLProcess(context.Background(), processRequest{Path: path, Allowed: path, SQL: query, Limit: 1})
		os.Exit(0)
	}
	path := darwinProcessPath(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestDarwinSQLProcessParentDeathReaps$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), "LANTERN_SQL_PARENT_DEATH=1", "LANTERN_SQL_DB="+path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var worker int
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = syscall.Kill(cmd.Process.Pid, syscall.SIGKILL)
			_, _ = cmd.Process.Wait()
		}
		if worker != 0 {
			_ = syscall.Kill(-worker, syscall.SIGKILL)
			_ = syscall.Kill(worker, syscall.SIGKILL)
		}
	})
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		pids := darwinChildWorkers(cmd.Process.Pid)
		if len(pids) > 0 {
			worker = pids[0]
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if worker == 0 {
		t.Fatal("worker did not start before parent death")
	}
	time.Sleep(100 * time.Millisecond)
	if err := syscall.Kill(cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		t.Fatal(err)
	}
	_, _ = cmd.Process.Wait()
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(worker, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("worker %d survived parent death", worker)
}

func TestDarwinSQLProcessFailureClassification(t *testing.T) {
	path := darwinProcessPath(t)
	req := processRequest{Path: path, Allowed: path, SQL: "SELECT 1", Limit: 1}
	ready := "printf '{\"version\":1,\"phase\":\"ready\",\"stats\":{\"pid\":%s,\"memory_limit_bytes\":" + strconv.FormatUint(ProcessMemoryBytes, 10) + "}}\\n' \"$$\"; "
	for _, tc := range []struct{ name, script, code string }{
		{"startup-unavailable", "exit 125", CodeWorkerUnavailable},
		{"ordinary-exit", ready + "printf 'panic: unrelated bug\\n' >&2; exit 2", CodeWorkerFailed},
		{"segmentation-fault", ready + "kill -SEGV $$", CodeWorkerFailed},
		{"external-kill", ready + "kill -KILL $$", CodeWorkerFailed},
		{"corrupt-protocol", ready + "printf 'invalid-json'; exit 0", CodeWorkerFailed},
		{"incidental-memory-message", ready + "printf 'a query string says out of memory\\n' >&2; exit 2", CodeWorkerFailed},
		{"oom-before-confirmed-limit", "printf 'fatal error: out of memory\\n' >&2; exit 2", CodeWorkerFailed},
		{"runtime-oom", ready + "printf 'fatal error: out of memory\\n' >&2; exit 2", CodeResultTooLarge},
		{"runtime-allocation-failure", ready + "printf 'runtime: out of memory: cannot allocate block\\n' >&2; exit 2", CodeResultTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			res, stats, err := runSQLProcessCommand(ctx, req, exec.CommandContext(ctx, "/bin/sh", "-c", "ulimit -c 0; "+tc.script))
			assertDarwinCode(t, err, tc.code)
			if res.RowCount != 0 || len(res.Rows) != 0 {
				t.Fatalf("failed command returned rows: %+v", res)
			}
			assertDarwinReaped(t, stats)
		})
	}
}

func darwinWorkerPIDs() []int {
	out, err := exec.Command("pgrep", "-f", workerArg).Output()
	if err != nil {
		return nil
	}
	return parsePIDs(string(out))
}

func darwinChildWorkers(parent int) []int {
	if parent <= 0 {
		return nil
	}
	out, err := exec.Command("pgrep", "-P", strconv.Itoa(parent), "-f", workerArg).Output()
	if err != nil {
		return nil
	}
	return parsePIDs(string(out))
}

func parsePIDs(text string) []int {
	var pids []int
	for _, field := range strings.Fields(text) {
		n, err := strconv.Atoi(field)
		if err == nil && n > 0 {
			pids = append(pids, n)
		}
	}
	return pids
}
