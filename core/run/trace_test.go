package run_test

import (
	"database/sql"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/audit"
	"github.com/loveRyujin/lantern/core/run"
	"github.com/loveRyujin/lantern/core/workspace"
	_ "modernc.org/sqlite"
)

func TestTraceUnknownRunAndOtherWorkspaceLeakNothing(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	a, err := workspace.Create(ctx, root, "ws-1")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := workspace.Create(ctx, root, "ws-2")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	insertRun(t, a.Runs, "run-a", run.StateAwaitingApproval)
	ticket := seedTicket(t, a.Runs, "run-a", "t-a", []byte(`{"object":"`+secret+`"}`), time.Now().Add(time.Hour))

	for name, call := range map[string]func() (run.Trace, error){
		"不存在":     func() (run.Trace, error) { return run.ReadTrace(ctx, a.Runs, "ws-1", "run-missing") },
		"另一工作区的库": func() (run.Trace, error) { return run.ReadTrace(ctx, b.Runs, "ws-2", "run-a") },
	} {
		tr, err := call()
		if !errors.Is(err, run.ErrNotFound) {
			t.Fatalf("%s: err=%v", name, err)
		}
		if len(tr.Entries) != 0 || tr.State != "" {
			t.Fatalf("%s: 返回了内容 %+v", name, tr)
		}
	}

	// 确认单上的工作区和查询方不一致：整条拒绝，不给部分轨迹。
	tr, err := run.ReadTrace(ctx, a.Runs, "ws-2", "run-a")
	if !errors.Is(err, run.ErrTraceWorkspace) {
		t.Fatalf("err=%v", err)
	}
	if len(tr.Entries) != 0 || tr.State != "" {
		t.Fatalf("越权返回了内容 %+v", tr)
	}
	for _, leak := range []string{secret, ticket.ParamsHash, ticket.ID} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("错误文案带出了 %q: %v", leak, err)
		}
	}
}

func TestTraceCorruptRowsFailClosed(t *testing.T) {
	cases := map[string]func(t *testing.T, db *sql.DB){
		"事件载荷不是 JSON": func(t *testing.T, db *sql.DB) {
			mustExec(t, db, `INSERT INTO run_events (run_id, kind, payload, created_at) VALUES ('run-x', 'step', '{坏', ?)`, stamp())
		},
		"步骤时间损坏": func(t *testing.T, db *sql.DB) {
			mustExec(t, db, `
				INSERT INTO run_steps (id, run_id, ordinal, status, tool_name, input_json, output_json, created_at, updated_at)
				VALUES ('s', 'run-x', 0, 'done', 'run_sql', '{}', '', '昨天', ?)`, stamp())
		},
		"步骤状态未知": func(t *testing.T, db *sql.DB) {
			mustExec(t, db, `
				INSERT INTO run_steps (id, run_id, ordinal, status, tool_name, input_json, output_json, created_at, updated_at)
				VALUES ('s', 'run-x', 0, 'exploded', 'run_sql', '{}', '', ?, ?)`, stamp(), stamp())
		},
		"审计时间损坏": func(t *testing.T, db *sql.DB) {
			mustExec(t, db, `INSERT INTO audit (at, actor, action, detail, run_id) VALUES ('x', 'agent', 'tool_failed', '', 'run-x')`)
		},
	}
	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			h := openWorkspace(t)
			insertRun(t, h.Runs, "run-x", run.StateCompleted)
			corrupt(t, h.Runs)
			tr, err := run.ReadTrace(t.Context(), h.Runs, "ws-1", "run-x")
			if !errors.Is(err, run.ErrTraceCorrupt) {
				t.Fatalf("err=%v", err)
			}
			if len(tr.Entries) != 0 {
				t.Fatalf("损坏时返回了部分轨迹 %+v", tr.Entries)
			}
		})
	}
}

func TestTraceTransitionsComeFromEveryWriter(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	h, err := workspace.Create(ctx, root, "ws-1")
	if err != nil {
		t.Fatal(err)
	}
	insertRun(t, h.Runs, "run-crash", run.StateCreated)
	svc := run.New(h.Runs)
	if err := svc.Set(ctx, "run-crash", run.StateRunning); err != nil {
		t.Fatal(err)
	}
	if err := svc.Set(ctx, "run-crash", run.StateRunning); err != nil {
		t.Fatal(err)
	}
	if err := svc.Begin(ctx, "s-1", "run-crash", "export_data", []byte(`{"object":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	// 重新打开会跑 Recover：卡在 executing 的运行标成失败，由触发器记下迁移。
	h, err = workspace.Open(ctx, root, "ws-1")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	tr, err := run.ReadTrace(ctx, h.Runs, "ws-1", "run-crash")
	if err != nil {
		t.Fatal(err)
	}
	var moves []string
	var interrupted bool
	for _, e := range tr.Entries {
		switch e.Source {
		case run.TraceTransition:
			moves = append(moves, e.From+">"+e.To)
		case run.TraceAudit:
			interrupted = interrupted || e.Kind == run.AuditExecutingInterrupted
		}
	}
	if got := strings.Join(moves, ","); got != ">created,created>running,running>failed" {
		t.Fatalf("迁移 %s", got)
	}
	if tr.State != run.StateFailed || !interrupted {
		t.Fatalf("state=%s interrupted=%v", tr.State, interrupted)
	}
}

func TestTraceClaimsLegacyApprovalAuditByDetail(t *testing.T) {
	h := openWorkspace(t)
	insertRun(t, h.Runs, "run-1", run.StateFailed)
	insertRun(t, h.Runs, "run-10", run.StateFailed)
	mustExec(t, h.Runs, `INSERT INTO audit (at, actor, action, detail) VALUES (?, 'approval', 'corrupt', 'id=t run=run-1 from=pending action=corrupt')`, stamp())
	mustExec(t, h.Runs, `INSERT INTO audit (at, actor, action, detail) VALUES (?, 'approval', 'corrupt', 'id=u run=run-10 from=pending action=corrupt')`, stamp())
	tr, err := run.ReadTrace(t.Context(), h.Runs, "ws-1", "run-1")
	if err != nil {
		t.Fatal(err)
	}
	var details []string
	for _, e := range tr.Entries {
		if e.Source == run.TraceAudit {
			details = append(details, e.Detail)
		}
	}
	if len(details) != 1 || !strings.Contains(details[0], "id=t ") {
		t.Fatalf("旧审计认领 %q", details)
	}
}

func TestTraceTablesAreAppendOnly(t *testing.T) {
	h := openWorkspace(t)
	insertRun(t, h.Runs, "run-1", run.StateCreated)
	if err := audit.NewSQLite(h.Runs).Append(t.Context(), audit.Record{Action: "x", RunID: "run-1"}); err != nil {
		t.Fatal(err)
	}
	mustExec(t, h.Runs, `INSERT INTO run_events (run_id, kind, payload, created_at) VALUES ('run-1', 'step', '{}', ?)`, stamp())
	for _, q := range []string{
		`UPDATE run_events SET payload = '{"summary":"改掉"}'`,
		`DELETE FROM run_events`,
		`UPDATE run_transitions SET to_state = 'completed'`,
		`DELETE FROM run_transitions`,
		`UPDATE audit SET action = 'y'`,
		`DELETE FROM audit`,
	} {
		if _, err := h.Runs.ExecContext(t.Context(), q); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Fatalf("%s: err=%v", q, err)
		}
	}
}

// 同一时间戳的行没有跨表序号。创建排最前，终态排最后，中间保持来源顺序。
func TestTraceTiesKeepLifecycleOrder(t *testing.T) {
	ctx := t.Context()
	h := openWorkspace(t)
	at := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	ts := at.Format(time.RFC3339Nano)
	mustExec(t, h.Runs, `INSERT INTO runs (id, state, version, context, created_at, updated_at) VALUES ('run-1', 'created', 0, '', ?, ?)`, ts, ts)
	svc := run.New(h.Runs)
	svc.SetClock(func() time.Time { return at })
	if err := svc.Set(ctx, "run-1", run.StateRunning); err != nil {
		t.Fatal(err)
	}
	mustExec(t, h.Runs, `INSERT INTO run_events (run_id, kind, payload, created_at) VALUES ('run-1', 'model_call', '{"summary":"最终回答 1 字"}', ?)`, ts)
	for _, st := range []string{run.StateAwaitingApproval, run.StateRunning, run.StateCompleted} {
		if err := svc.Set(ctx, "run-1", st); err != nil {
			t.Fatal(err)
		}
	}
	tr, err := run.ReadTrace(ctx, h.Runs, "ws-1", "run-1")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range tr.Entries {
		got = append(got, e.Source+":"+e.Kind)
	}
	if strings.Join(got, ",") != "transition:created,transition:running,transition:awaiting_approval,transition:running,model:model_call,transition:completed" {
		t.Fatalf("同一时间戳顺序 %v", got)
	}
}

// 迁移 006 之前创建的运行没有迁移记录，轨迹要明说，而不是静默缺行。
func TestTraceFlagsRunWithoutTransitions(t *testing.T) {
	h := openWorkspace(t)
	mustExec(t, h.Runs, `DROP TRIGGER runs_record_created`)
	insertRun(t, h.Runs, "legacy", run.StateCompleted)
	insertRun(t, h.Runs, "fresh", run.StateCompleted)
	tr, err := run.ReadTrace(t.Context(), h.Runs, "ws-1", "legacy")
	if err != nil || !tr.NoTransitions || tr.State != run.StateCompleted {
		t.Fatalf("tr=%+v err=%v", tr, err)
	}
}

// ReadTrace 在只读连接上也能完整跑完，说明它不需要写库。
func TestTraceWorksOnReadOnlyConnection(t *testing.T) {
	ctx := t.Context()
	h := openWorkspace(t)
	insertRun(t, h.Runs, "run-1", run.StateCreated)
	if err := run.New(h.Runs).Set(ctx, "run-1", run.StateCompleted); err != nil {
		t.Fatal(err)
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Join(h.Dir, "runs.db")), RawQuery: "mode=ro"}
	ro, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, err := ro.ExecContext(ctx, `INSERT INTO audit (at, actor, action) VALUES ('x', 'y', 'z')`); err == nil {
		t.Fatal("只读连接居然能写")
	}
	tr, err := run.ReadTrace(ctx, ro, "ws-1", "run-1")
	if err != nil || tr.State != run.StateCompleted || len(tr.Entries) != 2 {
		t.Fatalf("tr=%+v err=%v", tr, err)
	}
	list, err := run.ListRuns(ctx, ro, 10)
	if err != nil || len(list) != 1 || list[0].ID != "run-1" {
		t.Fatalf("list=%+v err=%v", list, err)
	}
}

// 审计层没有改写和删除路径：应用层只有 Append，源码里没有对审计与迁移表的 UPDATE/DELETE，
// 轨迹查询本身只有 SELECT。触发器是最后一道保证。
func TestNoAuditUpdateOrDeletePaths(t *testing.T) {
	typ := reflect.TypeOf(&audit.SQLite{})
	if typ.NumMethod() != 1 || typ.Method(0).Name != "Append" {
		t.Fatalf("audit.SQLite 暴露了 Append 以外的方法: %d", typ.NumMethod())
	}
	write := regexp.MustCompile(`(?is)\b(update\s+(audit|run_transitions|run_events)\b|delete\s+from\s+(audit|run_transitions|run_events)\b)`)
	root := moduleRoot(t)
	var hits []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if strings.HasSuffix(name, "_test.go") || !(strings.HasSuffix(name, ".go") || strings.HasSuffix(name, ".sql")) {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if loc := write.FindIndex(body); loc != nil {
			hits = append(hits, path+": "+string(body[loc[0]:loc[1]]))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) > 0 {
		t.Fatalf("审计层出现改写或删除: %v", hits)
	}
	body, err := os.ReadFile(filepath.Join(root, "core", "run", "trace.go"))
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`(?i)\b(insert\s+into|update\s+\w+\s+set|delete\s+from|replace\s+into)\b`).Match(body) {
		t.Fatal("trace.go 出现了写语句")
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("找不到 go.mod")
		}
		dir = parent
	}
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), q, args...); err != nil {
		t.Fatal(err)
	}
}

func stamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }
