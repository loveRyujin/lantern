package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/approval"
	"github.com/loveRyujin/lantern/core/llm"
	"github.com/loveRyujin/lantern/core/run"
	"github.com/loveRyujin/lantern/core/tools"
	"github.com/loveRyujin/lantern/core/workspace"
)

func TestRestartAwaitingApprovalThenCompletesOnce(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	ws, err := workspace.Create(ctx, root, "ws-1")
	if err != nil {
		t.Fatal(err)
	}
	insertRestartRun(t, ws, "run-1")
	args := `{"object":"13800138000"}`
	exp := suspendExport(t, ws, args)
	if exp.n() != 0 {
		t.Fatalf("挂起前执行了 %d 次", exp.n())
	}
	if err := ws.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := workspace.Open(ctx, root, "ws-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if got := restartState(t, reopened, "run-1"); got != run.StateAwaitingApproval {
		t.Fatalf("重启后状态 %s", got)
	}
	svc := run.New(reopened.Runs)
	events := restartEvents(t, svc, "run-1")
	if !restartHas(events, run.AuditApprovalRestored) {
		t.Fatalf("审计 %+v", events)
	}
	assertRestartClean(t, events)
	if exp.n() != 0 {
		t.Fatal("重启执行了敏感工具")
	}

	svc.SetApprover(restartGuard{n: exp.n, inner: &approval.Fake{Responses: []approval.Response{{
		Action: approval.ActionApprove,
	}}}})
	svc.SetExecutor(exp.Execute)
	res, err := svc.Resume(ctx, "run-1")
	if err != nil || res.State != run.StateCompleted || !res.Executed || exp.n() != 1 {
		t.Fatalf("resume %+v %v exec=%d", res, err, exp.n())
	}
	if string(exp.calls[0].Args) != args {
		t.Fatalf("执行参数 %s", exp.calls[0].Args)
	}
	if _, err := svc.Resume(ctx, "run-1"); !errors.Is(err, run.ErrNotAwaiting) || exp.n() != 1 {
		t.Fatalf("第二次 %v exec=%d", err, exp.n())
	}
}

func TestRestartHashMismatchRefusesLoopTicket(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	ws, err := workspace.Create(ctx, root, "ws-1")
	if err != nil {
		t.Fatal(err)
	}
	insertRestartRun(t, ws, "run-1")
	exp := suspendExport(t, ws, `{"object":"13800138000"}`)
	if err := ws.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := workspace.Open(ctx, root, "ws-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if _, err := reopened.Runs.ExecContext(ctx, `
		UPDATE approvals SET params_json = ? WHERE run_id = ?`, `{"object":"tampered"}`, "run-1"); err != nil {
		t.Fatal(err)
	}
	svc := run.New(reopened.Runs)
	svc.SetApprover(&approval.Fake{Responses: []approval.Response{{Action: approval.ActionApprove}}})
	svc.SetExecutor(exp.Execute)
	res, err := svc.Resume(ctx, "run-1")
	if !errors.Is(err, approval.ErrSnapshotMismatch) || res.Executed || res.State != run.StateFailed || exp.n() != 0 {
		t.Fatalf("resume %+v %v exec=%d", res, err, exp.n())
	}
	events := restartEvents(t, svc, "run-1")
	if !restartHas(events, run.AuditHashMismatch) {
		t.Fatalf("审计 %+v", events)
	}
	assertRestartClean(t, events)
}

func TestStepIsExecutingBeforeTool(t *testing.T) {
	ctx := t.Context()
	ws, err := workspace.Create(ctx, t.TempDir(), "order")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	insertRestartRun(t, ws, "run-1")
	svc := run.New(ws.Runs)
	status, steps := Persist(svc)
	h := newHarness(t, echoSpec(), goodStep("e", "ok"), llm.Say("完成"))
	h.loop.Status = status
	h.loop.Steps = steps
	var during string
	h.echo.fn = func(context.Context, tools.Call) (tools.Result, error) {
		if err := ws.Runs.QueryRowContext(ctx, `SELECT status FROM run_steps WHERE id = ?`, "e").Scan(&during); err != nil {
			t.Fatal(err)
		}
		return tools.Result{Summary: "已完成"}, nil
	}
	out, err := h.run()
	if err != nil || out.State != StateCompleted || h.echo.n() != 1 || during != run.StepExecuting {
		t.Fatalf("out=%+v err=%v exec=%d during=%s", out, err, h.echo.n(), during)
	}
	var after string
	if err := ws.Runs.QueryRowContext(ctx, `SELECT status FROM run_steps WHERE id = ?`, "e").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != run.StepDone || restartState(t, ws, "run-1") != run.StateCompleted {
		t.Fatalf("after=%s state=%s", after, restartState(t, ws, "run-1"))
	}
}

func suspendExport(t *testing.T, ws *workspace.Handle, args string) *fnTool {
	t.Helper()
	step := llm.CallTools(llm.ToolCall{ID: "e", Name: "export_data", Arguments: args})
	h := newHarness(t, echoSpec(), step)
	exp := h.add(tools.SpecExportData)
	h.approver.Responses = []approval.Response{{Action: approval.ActionDefer}}
	status, steps := Persist(run.New(ws.Runs))
	h.loop.Tickets = approval.New(ws.Runs)
	h.loop.Status = status
	h.loop.Steps = steps
	out, err := h.loop.Run(t.Context(), Input{RunID: "run-1", WorkspaceID: "ws-1", Prompt: "导出联系人"})
	if err != nil || out.State != StateAwaitingApproval || exp.n() != 0 {
		t.Fatalf("挂起 %+v %v exec=%d", out, err, exp.n())
	}
	return exp
}

func insertRestartRun(t *testing.T, ws *workspace.Handle, id string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := ws.Runs.ExecContext(t.Context(), `
		INSERT INTO runs (id, state, version, context, created_at, updated_at)
		VALUES (?, 'created', 0, '', ?, ?)`, id, now, now); err != nil {
		t.Fatal(err)
	}
}

func restartState(t *testing.T, ws *workspace.Handle, id string) string {
	t.Helper()
	var state string
	if err := ws.Runs.QueryRowContext(t.Context(), `SELECT state FROM runs WHERE id = ?`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func restartEvents(t *testing.T, svc *run.Service, runID string) []run.Event {
	t.Helper()
	ev, err := svc.Events(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func restartHas(events []run.Event, action string) bool {
	for _, ev := range events {
		if ev.Action == action {
			return true
		}
	}
	return false
}

func assertRestartClean(t *testing.T, events []run.Event) {
	t.Helper()
	for _, ev := range events {
		if strings.Contains(ev.Detail, "13800138000") || strings.Contains(ev.Detail, "tampered") {
			t.Fatalf("审计含参数 %+v", ev)
		}
	}
}

type restartGuard struct {
	n     func() int
	inner approval.Approver
}

func (g restartGuard) Request(ctx context.Context, ticket approval.Ticket) (approval.Decision, error) {
	if g.n() != 0 {
		return approval.Decision{}, errors.New("确认前工具已执行")
	}
	return g.inner.Request(ctx, ticket)
}
