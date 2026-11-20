package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/approval"
	"github.com/loveRyujin/lantern/core/audit"
	"github.com/loveRyujin/lantern/core/events"
	"github.com/loveRyujin/lantern/core/llm"
	"github.com/loveRyujin/lantern/core/policy"
	"github.com/loveRyujin/lantern/core/run"
	"github.com/loveRyujin/lantern/core/sqlx"
	"github.com/loveRyujin/lantern/core/tools"
	"github.com/loveRyujin/lantern/core/workspace"
)

// 这一组测试只让模型是脚本。run_sql 是生产 sqlx 工具（Linux/Darwin 走隔离 worker），
// 确认单、步骤、事件、审计都由生产实现写进 runs.db。轨迹只从库里读，测试不写任何步骤行。

const traceWS = "trace-ws"

type traceRig struct {
	t      *testing.T
	root   string
	ws     *workspace.Handle
	loop   *Loop
	sql    *gateTool
	export *execCounter
	runID  string
}

func newTraceRig(t *testing.T, approver approval.Approver, steps ...llm.Step) *traceRig {
	t.Helper()
	root := t.TempDir()
	ws, err := workspace.Create(t.Context(), root, traceWS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	path := filepath.Join(ws.Dir, "analytics.db")
	db, err := sqlx.Open(t.Context(), path, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	inner, err := sqlx.NewTool(db, path, path, sqlAudit{audit.NewSQLite(ws.Runs)})
	if err != nil {
		t.Fatal(err)
	}
	reg := &tools.Registry{}
	gate := &gateTool{inner: inner}
	export := &execCounter{}
	for _, tool := range []tools.Tool{gate, export} {
		if err := reg.Register(tool); err != nil {
			t.Fatal(err)
		}
	}
	status, stepStore := Persist(run.New(ws.Runs))
	r := &traceRig{t: t, root: root, ws: ws, sql: gate, export: export, runID: "run-trace"}
	r.loop = &Loop{
		Provider: llm.NewMock(llm.MustNewScript(steps...)),
		Tools:    reg,
		Policy:   policy.Default(),
		Approver: approver,
		Tickets:  approval.New(ws.Runs),
		Events:   &events.Recorder{},
		Log:      NewSQLiteLog(ws.Runs),
		Audit:    audit.NewSQLite(ws.Runs),
		Status:   status,
		Steps:    stepStore,
		Config:   Config{StepTimeout: 20 * time.Second, TotalTimeout: 40 * time.Second},
	}
	// 与桌面 LoopRunner 一样，先落一条 created 的运行行再进循环。
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := ws.Runs.ExecContext(t.Context(), `
		INSERT INTO runs (id, state, version, context, created_at, updated_at)
		VALUES (?, 'created', 0, '', ?, ?)`, r.runID, now, now); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *traceRig) run() Outcome {
	r.t.Helper()
	out, _ := r.loop.Run(context.Background(), Input{RunID: r.runID, WorkspaceID: traceWS, Prompt: "上个月导出客户联系方式"})
	return out
}

// restart 关库再用 workspace.Open 打开，和应用重启一样会跑 Recover。
func (r *traceRig) restart(root string) *workspace.Handle {
	r.t.Helper()
	if err := r.ws.Close(); err != nil {
		r.t.Fatal(err)
	}
	h, err := workspace.Open(r.t.Context(), root, traceWS)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = h.Close() })
	return h
}

func TestTraceOfRealLoopWithSQLAndApproval(t *testing.T) {
	exportArgs := `{"object":"contacts","fields":["phone"]}`
	r := newTraceRig(t, &approval.Fake{Responses: []approval.Response{{Action: approval.ActionApprove}}},
		sqlStep("sql-1", "SELECT COUNT(*) AS n FROM users"),
		llm.CallTools(llm.ToolCall{ID: "exp-1", Name: "export_data", Arguments: exportArgs}),
		llm.Say("已导出"),
	)
	out := r.run()
	if out.State != StateCompleted || out.ModelCalls != 3 || r.sql.n.Load() != 1 || r.export.n.Load() != 1 {
		t.Fatalf("%+v sql=%d export=%d", out, r.sql.n.Load(), r.export.n.Load())
	}
	h := r.restart(r.root)
	tr, err := run.ReadTrace(t.Context(), h.Runs, traceWS, r.runID)
	if err != nil {
		t.Fatal(err)
	}
	assertSorted(t, tr)
	got := shape(tr)
	want := []string{
		"transition:created", "transition:running",
		"model:model_call", "tool:run_sql",
		"model:model_call", "approval:requested", "approval:decided", "approval:consumed", "tool:export_data",
		"model:model_call", "transition:completed",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("轨迹顺序\n got %v\nwant %v", got, want)
	}

	// 与库里的原始行逐项对照。
	wantHash, err := approval.ParamsHash([]byte(exportArgs))
	if err != nil {
		t.Fatal(err)
	}
	var dbHash, dbDecision, ticketID string
	if err := h.Runs.QueryRowContext(t.Context(), `SELECT id, params_hash, decision FROM approvals WHERE run_id = ?`, r.runID).
		Scan(&ticketID, &dbHash, &dbDecision); err != nil {
		t.Fatal(err)
	}
	if dbHash != wantHash || dbDecision != "approve" {
		t.Fatalf("approvals hash=%s decision=%s", dbHash, dbDecision)
	}
	stepIDs := queryStrings(t, h.Runs, `SELECT id FROM run_steps WHERE run_id = ? ORDER BY ordinal`, r.runID)
	var toolIDs []string
	for _, e := range tr.Entries {
		switch e.Source {
		case run.TraceTool:
			toolIDs = append(toolIDs, e.StepID)
			if e.Status != run.StepDone || e.Summary == "" || e.Duration <= 0 || len(e.ParamsHash) != 64 {
				t.Fatalf("工具步骤 %+v", e)
			}
			if e.Tool == "export_data" && (e.Input != exportArgs || e.TicketID != ticketID || e.ParamsHash != wantHash) {
				t.Fatalf("导出步骤 %+v", e)
			}
			if e.Tool == "run_sql" && !strings.Contains(e.Input, "SELECT COUNT(*)") {
				t.Fatalf("SQL 参数 %q", e.Input)
			}
		case run.TraceApproval:
			if e.ParamsHash != dbHash || e.TicketID != ticketID {
				t.Fatalf("确认事件 %+v", e)
			}
			if e.Kind == run.ApprovalDecided && e.Decision != "approve" {
				t.Fatalf("决定 %+v", e)
			}
		case run.TraceModel:
			if e.Status != "ok" || e.Summary == "" {
				t.Fatalf("模型调用 %+v", e)
			}
		}
	}
	if strings.Join(toolIDs, ",") != strings.Join(stepIDs, ",") || strings.Join(stepIDs, ",") != "sql-1,exp-1" {
		t.Fatalf("轨迹步骤 %v，run_steps %v", toolIDs, stepIDs)
	}
	var events, audits int
	if err := h.Runs.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM run_events WHERE run_id = ?`, r.runID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := h.Runs.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM audit WHERE run_id = ?`, r.runID).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if events != out.ModelCalls+2 || audits != 0 {
		t.Fatalf("run_events=%d audit=%d", events, audits)
	}
	if r.sql.n.Load() != 1 || r.export.n.Load() != 1 {
		t.Fatal("查询轨迹触发了工具")
	}
}

func TestTraceOfRealLoopFailureShowsRejection(t *testing.T) {
	r := newTraceRig(t, &approval.Fake{Responses: []approval.Response{{Action: approval.ActionReject}}},
		sqlStep("sql-w", "DELETE FROM users"),
		llm.CallTools(llm.ToolCall{ID: "exp-1", Name: "export_data", Arguments: `{"object":"contacts"}`}),
		llm.Say("不该走到这里"),
	)
	out := r.run()
	if out.State != StateRejected || r.export.n.Load() != 0 {
		t.Fatalf("%+v export=%d", out, r.export.n.Load())
	}
	h := r.restart(r.root)
	tr, err := run.ReadTrace(t.Context(), h.Runs, traceWS, r.runID)
	if err != nil {
		t.Fatal(err)
	}
	assertSorted(t, tr)
	var sqlStep, rejected, sqlAuditSeen bool
	for _, e := range tr.Entries {
		switch {
		case e.Source == run.TraceTool && e.Tool == "export_data":
			t.Fatalf("拒绝后仍有导出步骤 %+v", e)
		case e.Source == run.TraceTool && e.Tool == "run_sql":
			sqlStep = e.Status == run.StepError && e.Code == sqlx.CodeRejected && strings.Contains(e.Input, "DELETE FROM users")
		case e.Source == run.TraceApproval && e.Kind == run.ApprovalDecided:
			rejected = e.Decision == "reject" && len(e.ParamsHash) == 64
		case e.Source == run.TraceApproval && e.Kind == run.ApprovalConsumed:
			t.Fatalf("拒绝后被消费 %+v", e)
		case e.Source == run.TraceAudit && e.Kind == sqlx.CodeRejected:
			sqlAuditSeen = true
		}
	}
	last := tr.Entries[len(tr.Entries)-1]
	if !sqlStep || !rejected || !sqlAuditSeen || last.Source != run.TraceTransition || last.From != run.StateRunning || last.To != run.StateRejected {
		t.Fatalf("sql=%v rejected=%v audit=%v last=%+v\n%v", sqlStep, rejected, sqlAuditSeen, last, shape(tr))
	}
}

func TestTraceOfRealLoopAwaitingApprovalAfterRestart(t *testing.T) {
	r := newTraceRig(t, &approval.Fake{Responses: []approval.Response{{Action: approval.ActionDefer}}},
		llm.CallTools(llm.ToolCall{ID: "exp-1", Name: "export_data", Arguments: `{"object":"contacts"}`}),
	)
	out := r.run()
	if out.State != StateAwaitingApproval || r.export.n.Load() != 0 {
		t.Fatalf("%+v export=%d", out, r.export.n.Load())
	}
	h := r.restart(r.root)
	tr, err := run.ReadTrace(t.Context(), h.Runs, traceWS, r.runID)
	if err != nil {
		t.Fatal(err)
	}
	assertSorted(t, tr)
	want := "transition:created,transition:running,model:model_call,approval:requested,transition:awaiting_approval,audit:approval_restored"
	if got := strings.Join(shape(tr), ","); got != want || tr.State != run.StateAwaitingApproval {
		t.Fatalf("state=%s\n got %s\nwant %s", tr.State, got, want)
	}
	for _, e := range tr.Entries {
		if e.Source == run.TraceApproval && (e.Status != "pending" || len(e.ParamsHash) != 64) {
			t.Fatalf("待确认 %+v", e)
		}
	}
}

// 执行中崩溃：工具正在跑的时候把 runs.db 冻结成另一份磁盘，等同于进程在这一刻退出。
// 用这份磁盘重启，Recover 把 executing 标成失败，工具不重跑。
func TestTraceOfRealLoopCrashedWhileExecuting(t *testing.T) {
	r := newTraceRig(t, &approval.Fake{},
		sqlStep("sql-1", "SELECT 1"),
		llm.Say("完成"),
	)
	r.sql.entered = make(chan struct{})
	r.sql.release = make(chan struct{})
	done := make(chan Outcome, 1)
	go func() { done <- r.run() }()
	select {
	case <-r.sql.entered:
	case <-time.After(20 * time.Second):
		t.Fatal("工具没有开始执行")
	}
	crashRoot := t.TempDir()
	snapshotWorkspace(t, r.ws, filepath.Join(crashRoot, "workspaces", traceWS))
	close(r.sql.release)
	if out := <-done; out.State != StateCompleted {
		t.Fatalf("原进程 %+v", out)
	}
	executed := r.sql.n.Load()

	h, err := workspace.Open(t.Context(), crashRoot, traceWS)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	tr, err := run.ReadTrace(t.Context(), h.Runs, traceWS, r.runID)
	if err != nil {
		t.Fatal(err)
	}
	assertSorted(t, tr)
	want := "transition:created,transition:running,model:model_call,tool:run_sql,transition:failed,audit:executing_interrupted"
	if got := strings.Join(shape(tr), ","); got != want || tr.State != run.StateFailed {
		t.Fatalf("state=%s\n got %s\nwant %s", tr.State, got, want)
	}
	for _, e := range tr.Entries {
		if e.Source == run.TraceTool && (e.Status != run.StepExecuting || e.Summary != "") {
			t.Fatalf("崩溃时的步骤 %+v", e)
		}
		if e.Source == run.TraceTransition && e.To == run.StateFailed && e.From != run.StateRunning {
			t.Fatalf("失败迁移 %+v", e)
		}
	}
	if r.sql.n.Load() != executed || executed != 1 {
		t.Fatalf("重启后工具执行 %d 次，崩溃前 %d 次", r.sql.n.Load(), executed)
	}
}

func shape(tr run.Trace) []string {
	out := make([]string, len(tr.Entries))
	for i, e := range tr.Entries {
		out[i] = e.Source + ":" + e.Kind
	}
	return out
}

func assertSorted(t *testing.T, tr run.Trace) {
	t.Helper()
	if len(tr.Entries) == 0 {
		t.Fatal("轨迹为空")
	}
	if !sort.SliceIsSorted(tr.Entries, func(i, j int) bool { return tr.Entries[i].At.Before(tr.Entries[j].At) }) {
		t.Fatalf("没有按时间排序: %v", shape(tr))
	}
}

func queryStrings(t *testing.T, db *sql.DB, q string, args ...any) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// snapshotWorkspace 复制工作区目录，runs.db 用 VACUUM INTO 取一致快照。
func snapshotWorkspace(t *testing.T, ws *workspace.Handle, dst string) {
	t.Helper()
	err := filepath.WalkDir(ws.Dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(ws.Dir, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		name := d.Name()
		if strings.HasPrefix(name, "runs.db") || strings.HasSuffix(name, "-shm") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, body, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	// 生产连接装了授权器，不允许 VACUUM。另开一条只读连接取快照。
	src := (&url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Join(ws.Dir, "runs.db")), RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", src)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(t.Context(), `VACUUM INTO ?`, filepath.Join(dst, "runs.db")); err != nil {
		t.Fatal(err)
	}
}

// gateTool 包住生产 run_sql，只加执行计数和可选的停顿点。
type gateTool struct {
	inner   tools.Tool
	n       atomic.Int64
	entered chan struct{}
	release chan struct{}
}

func (g *gateTool) Spec() tools.Spec { return g.inner.Spec() }

func (g *gateTool) Execute(ctx context.Context, call tools.Call) (tools.Result, error) {
	g.n.Add(1)
	if g.entered != nil {
		close(g.entered)
		<-g.release
	}
	return g.inner.Execute(ctx, call)
}

type execCounter struct{ n atomic.Int64 }

func (e *execCounter) Spec() tools.Spec { return tools.SpecExportData }

func (e *execCounter) Execute(ctx context.Context, call tools.Call) (tools.Result, error) {
	if err := ctx.Err(); err != nil {
		return tools.Result{}, err
	}
	e.n.Add(1)
	raw, _ := json.Marshal(map[string]any{"exported": call.ApprovalID != ""})
	return tools.Result{Content: string(raw), Summary: "已导出"}, nil
}

type sqlAudit struct{ log audit.Log }

func (s sqlAudit) Append(ctx context.Context, ev sqlx.Event) error {
	return s.log.Append(ctx, audit.Record{Actor: ev.Actor, Action: ev.Action, RunID: ev.RunID, Detail: ev.Detail})
}
