//go:build linux

package sqlx_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/approval"
	"github.com/loveRyujin/lantern/core/audit"
	"github.com/loveRyujin/lantern/core/sqlx"
	"github.com/loveRyujin/lantern/core/tools"
	"github.com/loveRyujin/lantern/core/workspace"
)

// This uses real workspace SQLite handles: a SQL worker's resource limit must
// never become a process-wide limit on concurrent audit or approval writes.
func TestSQLWorkerMemoryPressureDoesNotBlockWorkspaceWrites(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	w, err := workspace.Create(ctx, t.TempDir(), "sql-memory-isolation")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	path := filepath.Join(w.Dir, "analytics.db")
	db, err := sqlx.Open(ctx, path, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tool, err := sqlx.NewTool(db, path, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	params := []byte(`{"object":"contacts"}`)
	hash, err := approval.ParamsHash(params)
	if err != nil {
		t.Fatal(err)
	}
	ticket := approval.Ticket{ID: "memory-approval", RunID: "memory-run", StepID: "memory-step", WorkspaceID: w.ID, Action: "export_data", ExportObject: "contacts", Params: params, ParamsHash: hash}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := w.Runs.ExecContext(ctx, `INSERT INTO runs (id,state,version,context,created_at,updated_at) VALUES (?, 'running',0,'',?,?)`, ticket.RunID, now, now); err != nil {
		t.Fatal(err)
	}
	cols := strings.TrimSuffix(strings.Repeat("hex(zeroblob(2000000)),", 200), ",")
	args, err := json.Marshal(map[string]any{"sql": "WITH x AS MATERIALIZED (SELECT " + cols + ") SELECT count(*) FROM x"})
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		result tools.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() { result, err := tool.Execute(ctx, tools.Call{Args: args}); done <- outcome{result, err} }()
	// Wait until a genuine worker is already consuming intermediate-value memory,
	// rather than merely scheduling the goroutine before the parent writes.
	var pid int
	for pid == 0 {
		pid = sqlWorkerUnderPressure()
		if pid != 0 {
			break
		}
		select {
		case out := <-done:
			t.Fatalf("worker finished before concurrent writes: %v", out.err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	if err := audit.NewSQLite(w.Runs).Append(ctx, audit.Record{Actor: "test", Action: "during_sql_pressure", RunID: ticket.RunID, Detail: "parent process remains writable"}); err != nil {
		t.Fatal(err)
	}
	machine := approval.New(w.Runs)
	if err := machine.Create(ctx, ticket); err != nil {
		t.Fatal(err)
	}
	if _, err := machine.Decide(ctx, ticket.ID, approval.Decision{Action: approval.ActionApprove}); err != nil {
		t.Fatal(err)
	}
	// The writes must complete while the worker is still alive, not after an OOM
	// releases an accidentally shared process-wide heap limit.
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err != nil {
		t.Fatalf("worker stopped before parent writes completed: %v", err)
	}
	out := <-done
	var te *tools.Error
	if !errors.As(out.err, &te) || te.Code != sqlx.CodeResultTooLarge || out.result.Content != "" {
		t.Fatalf("worker outcome: %+v error=%v", out.result, out.err)
	}
	var count int
	if err := w.Runs.QueryRowContext(ctx, `SELECT count(*) FROM audit WHERE action='during_sql_pressure'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("persisted audit count=%d error=%v", count, err)
	}
	var state string
	if err := w.Runs.QueryRowContext(ctx, `SELECT state FROM approvals WHERE id=?`, ticket.ID).Scan(&state); err != nil || state != "approved" {
		t.Fatalf("persisted approval=%s error=%v", state, err)
	}
	result, err := tool.Execute(ctx, tools.Call{Args: []byte(`{"sql":"SELECT 42"}`)})
	if err != nil {
		t.Fatalf("normal query after worker OOM: %v", err)
	}
	var parsed struct {
		Rows [][]int `json:"rows"`
	}
	if err := json.Unmarshal([]byte(result.Content), &parsed); err != nil || len(parsed.Rows) != 1 || len(parsed.Rows[0]) != 1 || parsed.Rows[0][0] != 42 {
		t.Fatalf("normal result=%s error=%v", result.Content, err)
	}
}

func sqlWorkerUnderPressure() int {
	files, _ := filepath.Glob("/proc/self/task/*/children")
	for _, file := range files {
		body, _ := os.ReadFile(file)
		for _, field := range strings.Fields(string(body)) {
			prefix := "/proc/" + field
			args, _ := os.ReadFile(prefix + "/cmdline")
			if !strings.Contains(string(args), "--lantern-sql-worker-v1") {
				continue
			}
			status, _ := os.ReadFile(prefix + "/status")
			for _, line := range strings.Split(string(status), "\n") {
				fields := strings.Fields(line)
				if len(fields) == 3 && fields[0] == "VmRSS:" {
					kb, _ := strconv.Atoi(fields[1])
					if kb > 64<<10 {
						pid, _ := strconv.Atoi(field)
						return pid
					}
				}
			}
		}
	}
	return 0
}
