//go:build linux

package sqlx_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/sqlx"
	"github.com/loveRyujin/lantern/core/tools"
	"github.com/loveRyujin/lantern/core/workspace"
)

// These cases exercise the worker's checks after Query returns. Mutating only
// after a child holds the database open and has spent several CPU ticks in a
// pure-expression query prevents the parent's preflight from satisfying them.
func TestSQLWorkerPostQueryIntegrity(t *testing.T) {
	for _, kind := range []string{"same-inode-changed-bytes", "same-bytes-new-inode"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			w, err := workspace.Create(ctx, t.TempDir(), "worker-integrity")
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			path := filepath.Join(w.Dir, "analytics.db")
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			identity, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			db, err := sqlx.Open(ctx, path, path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			tool, err := sqlx.NewTool(db, path, path, nil)
			if err != nil {
				t.Fatal(err)
			}
			type outcome struct {
				result tools.Result
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				// No table access, result materialization or sleeps: CPU growth
				// with the DB fd open identifies the actual recursive query.
				res, err := tool.Execute(ctx, tools.Call{Args: []byte(`{"sql":"WITH RECURSIVE x(n) AS (VALUES(0) UNION ALL SELECT n+1 FROM x WHERE n<3000000) SELECT sum(n) FROM x"}`)})
				done <- outcome{res, err}
			}()
			consumed := false
			defer func() {
				cancel()
				if !consumed {
					select {
					case <-done:
					case <-time.After(3 * time.Second):
						t.Error("SQL worker did not finish after cancellation")
					}
				}
			}()

			var pid, growth int
			var previous uint64
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			for growth < 3 {
				select {
				case out := <-done:
					consumed = true
					t.Fatalf("worker finished before query synchronization: result=%+v error=%v", out.result, out.err)
				case <-ctx.Done():
					t.Fatalf("waiting for running SQL worker: %v", ctx.Err())
				case <-ticker.C:
					current, ticks := integrityWorkerCPU(path)
					if current == 0 {
						continue
					}
					if current != pid {
						pid, previous, growth = current, ticks, 0
					} else if ticks > previous {
						previous = ticks
						growth++
					}
				}
			}
			t.Logf("worker pid=%d observed %d CPU tick increases with database fd open", pid, growth)

			switch kind {
			case "same-inode-changed-bytes":
				f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, writeErr := f.Write([]byte("worker-integrity-post-query-change"))
				closeErr := f.Close()
				if writeErr != nil || closeErr != nil {
					t.Fatalf("append: write=%v close=%v", writeErr, closeErr)
				}
			case "same-bytes-new-inode":
				replacement := path + ".replacement"
				if err := os.WriteFile(replacement, original, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(replacement, path); err != nil {
					t.Fatal(err)
				}
			}
			current, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			changed, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "same-inode-changed-bytes" {
				if !os.SameFile(identity, current) || bytes.Equal(original, changed) {
					t.Fatal("test did not change bytes while preserving the inode")
				}
			} else if os.SameFile(identity, current) || !bytes.Equal(original, changed) {
				t.Fatal("test did not replace the inode with identical bytes")
			}
			out := <-done
			consumed = true
			var te *tools.Error
			if !errors.As(out.err, &te) || te.Code != sqlx.CodeBusy || !te.Retryable || out.result.Content != "" {
				t.Fatalf("worker post-query integrity: result=%+v error=%v; want retryable data_busy and no content", out.result, out.err)
			}
		})
	}
}

// Returns only direct children running the SQL worker, holding this exact DB
// path open. /proc/PID/stat's utime+stime excludes time spent merely waiting.
func integrityWorkerCPU(path string) (pid int, ticks uint64) {
	files, _ := filepath.Glob("/proc/self/task/*/children")
	for _, file := range files {
		children, _ := os.ReadFile(file)
		for _, child := range strings.Fields(string(children)) {
			prefix := "/proc/" + child
			args, _ := os.ReadFile(prefix + "/cmdline")
			if !bytes.Contains(args, []byte("--lantern-sql-worker-v1")) {
				continue
			}
			fds, _ := filepath.Glob(prefix + "/fd/*")
			open := false
			for _, fd := range fds {
				target, _ := os.Readlink(fd)
				if target == path {
					open = true
					break
				}
			}
			if !open {
				continue
			}
			stat, _ := os.ReadFile(prefix + "/stat")
			end := bytes.LastIndexByte(stat, ')')
			if end < 0 {
				continue
			}
			fields := strings.Fields(string(stat[end+1:]))
			if len(fields) < 13 {
				continue
			}
			user, uerr := strconv.ParseUint(fields[11], 10, 64)
			system, serr := strconv.ParseUint(fields[12], 10, 64)
			id, perr := strconv.Atoi(child)
			if uerr == nil && serr == nil && perr == nil {
				return id, user + system
			}
		}
	}
	return 0, 0
}
