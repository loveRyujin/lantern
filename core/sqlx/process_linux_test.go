//go:build linux

package sqlx

import (
	"context"
	"database/sql"
	"encoding/base64"
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

	"github.com/loveRyujin/lantern/core/tools"
	"modernc.org/sqlite"
)

type parentMustNotQuery struct{ t *testing.T }

func (s parentMustNotQuery) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	s.t.Fatal("SQL was executed in the parent process")
	return nil, errors.New("parent query forbidden")
}

func processTestPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "analytics.db")
	createEmptyDB(t, path)
	return path
}

func assertProcessReaped(t *testing.T, stats processStats) {
	t.Helper()
	if stats.PID <= 0 || stats.PID == os.Getpid() {
		t.Fatalf("invalid worker PID: %+v", stats)
	}
	// A zombie still has /proc/PID. Cmd.Wait must finish before returning.
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", stats.PID)); !os.IsNotExist(err) {
		t.Fatalf("worker %d was not reaped: %v", stats.PID, err)
	}
}

func assertWorkerMemory(t *testing.T, stats processStats) {
	t.Helper()
	if stats.MemoryLimit != ProcessMemoryBytes {
		t.Fatalf("worker did not confirm RLIMIT_AS: %+v", stats)
	}
	if stats.PeakRSS == 0 || stats.PeakRSS > ProcessMemoryBytes || stats.PeakVirtual > ProcessMemoryBytes {
		t.Fatalf("worker memory exceeded its limit: %+v", stats)
	}
	assertProcessReaped(t, stats)
}

func assertProcessCode(t *testing.T, err error, want string) {
	t.Helper()
	var te *tools.Error
	if !errors.As(err, &te) || te.Code != want || te.Retryable {
		t.Fatalf("error = %v; want nonretryable %s", err, want)
	}
}

func TestSQLProcessIsolationAndIntegerPrecision(t *testing.T) {
	path := processTestPath(t)
	tool, err := NewTool(parentMustNotQuery{t}, path, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	query := "SELECT 9223372036854775807 AS hi, -9223372036854775808 AS lo, 9007199254740993 AS beyond_float"
	result, err := tool.Execute(t.Context(), sqlCall(t, query))
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Rows [][]json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal([]byte(result.Content), &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Rows) != 1 || len(parsed.Rows[0]) != 3 {
		t.Fatalf("unexpected result: %s", result.Content)
	}
	for i, want := range []string{"9223372036854775807", "-9223372036854775808", "9007199254740993"} {
		if string(parsed.Rows[0][i]) != want {
			t.Fatalf("integer[%d] = %s; want %s", i, parsed.Rows[0][i], want)
		}
	}
	_, stats, err := runSQLProcess(t.Context(), processRequest{Path: path, Allowed: path, SQL: "SELECT 1", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	assertWorkerMemory(t, stats)
}

func TestSQLProcessRejectsRegisteredDatabaseReplacement(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "rename"} {
		t.Run(kind, func(t *testing.T) {
			path := processTestPath(t)
			secret := filepath.Join(filepath.Dir(path), "sensitive.db")
			createEmptyDB(t, secret)
			writer := sql.OpenDB(hookConnector{dsn: secret, drv: &sqlite.Driver{}})
			if _, err := writer.ExecContext(t.Context(), "INSERT INTO t VALUES (777)"); err != nil {
				_ = writer.Close()
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			db, err := Open(t.Context(), path, path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			tool, err := NewTool(db, path, path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(path, path+".original"); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "symlink":
				err = os.Symlink(secret, path)
			case "hardlink":
				err = os.Link(secret, path)
			case "rename":
				err = os.Rename(secret, path)
			}
			if err != nil {
				t.Fatal(err)
			}
			res, err := tool.Execute(t.Context(), sqlCall(t, "SELECT id FROM t"))
			var te *tools.Error
			if !errors.As(err, &te) || te.Code != CodeBusy || !te.Retryable {
				t.Fatalf("registered inode replacement error=%v; want retryable data_busy", err)
			}
			if res.Content != "" {
				t.Fatalf("replacement leaked sensitive data: %s", res.Content)
			}
		})
	}
}

func TestSQLProcessB6IntermediateMemory(t *testing.T) {
	path := processTestPath(t)
	// Each expression is individually legal (4 MB < MaxValueBytes). The final
	// projection is small; only the intermediate values exhaust the worker.
	parts, names := make([]string, 200), make([]string, 200)
	for i := range parts {
		parts[i] = fmt.Sprintf("hex(zeroblob(2000000)) AS c%d", i)
		names[i] = fmt.Sprintf("c%d", i)
	}
	cols, group := strings.Join(parts, ","), strings.Join(names, ",")
	lengths := make([]string, len(names))
	for i, name := range names {
		lengths[i] = "length(" + name + ")"
	}
	lengthSum := strings.Join(lengths, "+")
	values := strings.TrimSuffix(strings.Repeat("hex(zeroblob(2000000)),", 200), ",")
	cases := []struct{ name, query string }{
		{"materialized-cte", "WITH x AS MATERIALIZED (SELECT " + cols + ") SELECT count(*) FROM x"},
		{"subquery", "SELECT " + lengthSum + " FROM (SELECT " + cols + " LIMIT 1 OFFSET 0)"},
		{"group-by", "SELECT count(*) FROM (SELECT " + cols + ") GROUP BY " + group},
		{"union", "SELECT count(*) FROM (SELECT " + cols + " UNION SELECT " + cols + ")"},
		{"in-list", "SELECT 1 WHERE 'a' IN (" + values + ")"},
		{"order-by-random", "SELECT " + lengthSum + " FROM (SELECT " + cols + " ORDER BY random() LIMIT 1 OFFSET 0)"},
	}
	pids := make(map[int]bool)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			res, stats, err := runSQLProcess(ctx, processRequest{Path: path, Allowed: path, SQL: tc.query, Limit: 1})
			t.Logf("pid=%d rss=%d virtual=%d error=%v", stats.PID, stats.PeakRSS, stats.PeakVirtual, err)
			assertProcessCode(t, err, CodeResultTooLarge)
			if res.RowCount != 0 || len(res.Rows) != 0 {
				t.Fatalf("failed worker returned rows: %+v", res)
			}
			assertWorkerMemory(t, stats)
			if stats.PeakRSS < 64<<20 {
				t.Fatalf("query did not reach intermediate memory pressure: %+v", stats)
			}
			if pids[stats.PID] {
				t.Fatalf("worker PID reused across memory cases: %d", stats.PID)
			}
			pids[stats.PID] = true
		})
	}
}

func TestSQLProcessTimeoutReapsAndRecovers(t *testing.T) {
	path := processTestPath(t)
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
	assertProcessReaped(t, stats)
	if err := syscall.Kill(stats.PID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("canceled worker still exists: %v", err)
	}
	res, next, err := runSQLProcess(t.Context(), processRequest{Path: path, Allowed: path, SQL: "SELECT 42", Limit: 1})
	if err != nil || res.RowCount != 1 {
		t.Fatalf("query after cancellation: result=%+v error=%v", res, err)
	}
	assertWorkerMemory(t, next)
}

func TestSQLProcessTextEncodingAndFinalJSONLimit(t *testing.T) {
	tool := openEmptyTool(t)
	t.Run("invalid-utf8", func(t *testing.T) {
		res, err := tool.Execute(t.Context(), sqlCall(t, "SELECT CAST(x'ff00fe6162' AS TEXT)"))
		if err != nil {
			t.Fatal(err)
		}
		var cell EncodedText
		if err := json.Unmarshal([]byte(firstCellRaw(t, res.Content)), &cell); err != nil {
			t.Fatal(err)
		}
		want := base64.StdEncoding.EncodeToString([]byte{0xff, 0, 0xfe, 'a', 'b'})
		if cell.Encoding != "base64" || cell.Data != want {
			t.Fatalf("cell=%+v; want base64 %s", cell, want)
		}
	})
	for _, tc := range []struct {
		name, query string
		size        int
		tooLarge    bool
	}{
		{"html-text-fits", "SELECT replace(hex(zeroblob(150000)), '0', '<>&')", 900000, false},
		{"control-text-fits", "SELECT replace(hex(zeroblob(80000)), '0', char(1))", 160000, false},
		{"control-text-over", "SELECT replace(hex(zeroblob(90000)), '0', char(1))", 0, true},
		{"unicode-separator-fits", "SELECT replace(hex(zeroblob(80000)), '0', char(8232))", 480000, false},
		{"unicode-separator-over", "SELECT replace(hex(zeroblob(90000)), '0', char(8232))", 0, true},
		{"invalid-text-over", "SELECT CAST(zeroblob(800000) || x'ff' AS TEXT)", 0, true},
		{"blob-fits", "SELECT zeroblob(" + strconv.Itoa(((MaxResultBytes-1024)/4)*3) + ")", ((MaxResultBytes - 1024) / 4) * 3, false},
		{"blob-over", "SELECT zeroblob(" + strconv.Itoa((MaxResultBytes/4)*3+1) + ")", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := tool.Execute(t.Context(), sqlCall(t, tc.query))
			if tc.tooLarge {
				assertProcessCode(t, err, CodeResultTooLarge)
				if res.Content != "" {
					t.Fatal("oversized result leaked content")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Content) > MaxResultBytes || !json.Valid([]byte(res.Content)) {
				t.Fatalf("invalid/oversized final JSON (%d bytes)", len(res.Content))
			}
			var cell string
			if err := json.Unmarshal([]byte(firstCellRaw(t, res.Content)), &cell); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(tc.name, "blob-") {
				raw, err := base64.StdEncoding.DecodeString(cell)
				if err != nil || len(raw) != tc.size {
					t.Fatalf("blob len=%d error=%v", len(raw), err)
				}
			} else if len(cell) != tc.size {
				t.Fatalf("text len=%d; want %d", len(cell), tc.size)
			}
		})
	}
	t.Run("later-oversized-row-discards-earlier-rows", func(t *testing.T) {
		res, err := tool.Execute(t.Context(), sqlCall(t, "SELECT 'safe' AS v UNION ALL SELECT replace(hex(zeroblob(90000)), '0', char(1))"))
		assertProcessCode(t, err, CodeResultTooLarge)
		if res.Content != "" {
			t.Fatal("earlier rows leaked after oversized later row")
		}
	})
	t.Run("multirow-final-json-cap", func(t *testing.T) {
		query := "WITH RECURSIVE x(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM x WHERE n<3) SELECT replace(hex(zeroblob(50000)), '0', char(1)) AS v FROM x"
		res, err := tool.Execute(t.Context(), sqlCall(t, query))
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Content) > MaxResultBytes {
			t.Fatalf("multirow final JSON exceeds cap: %d", len(res.Content))
		}
		var parsed Result
		if err := json.Unmarshal([]byte(res.Content), &parsed); err != nil {
			t.Fatal(err)
		}
		if parsed.RowCount != 1 || len(parsed.Rows) != 1 || !parsed.Truncated || parsed.TruncateReason != TruncateBytes {
			t.Fatalf("bad multirow byte truncation: rows=%d truncated=%v reason=%s", parsed.RowCount, parsed.Truncated, parsed.TruncateReason)
		}
	})
}

func TestSQLProcessFailureClassification(t *testing.T) {
	path := processTestPath(t)
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
			assertProcessCode(t, err, tc.code)
			if res.RowCount != 0 || len(res.Rows) != 0 {
				t.Fatalf("failed worker returned rows: %+v", res)
			}
			assertProcessReaped(t, stats)
		})
	}
	t.Run("executable-missing", func(t *testing.T) {
		_, _, err := runSQLProcessCommand(t.Context(), req, exec.CommandContext(t.Context(), filepath.Join(t.TempDir(), "missing-worker")))
		assertProcessCode(t, err, CodeWorkerUnavailable)
	})
	t.Run("database-missing", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "missing.db")
		_, stats, err := runSQLProcess(t.Context(), processRequest{Path: missing, Allowed: missing, SQL: "SELECT 1", Limit: 1})
		assertProcessCode(t, err, CodeWorkerUnavailable)
		if stats.PID != 0 {
			t.Fatalf("missing database should fail before starting a worker: %+v", stats)
		}
	})
}

func TestSQLProcessRegisteredFileReplacement(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "rename"} {
		t.Run(kind, func(t *testing.T) {
			path := processTestPath(t)
			other := filepath.Join(filepath.Dir(path), "replacement.db")
			createEmptyDB(t, other)
			tool, err := NewTool(parentMustNotQuery{t}, path, path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(path, path+".original"); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "symlink":
				err = os.Symlink(other, path)
			case "hardlink":
				err = os.Link(other, path)
			case "rename":
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
