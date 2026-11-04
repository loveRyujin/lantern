package run_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/approval"
	"github.com/loveRyujin/lantern/core/run"
	"github.com/loveRyujin/lantern/core/tools"
	"github.com/loveRyujin/lantern/core/workspace"
)

const secret = "13800138000"

func TestRecoverClassifiesWithoutExecuting(t *testing.T) {
	ctx := t.Context()
	h := openWorkspace(t)
	db := h.Runs
	exec := &counter{}

	insertRun(t, db, "awaiting", run.StateAwaitingApproval)
	seedTicket(t, db, "awaiting", "t-await", []byte(`{"object":"`+secret+`"}`), time.Now().Add(time.Hour))

	insertRun(t, db, "await-exec", run.StateAwaitingApproval)
	insertStep(t, db, "s-await-exec", "await-exec", 0, run.StepExecuting, "export_data", `{"object":"`+secret+`"}`)
	seedTicket(t, db, "await-exec", "t-await-exec", []byte(`{"object":"contacts"}`), time.Now().Add(time.Hour))

	insertRun(t, db, "mid-exec", run.StateRunning)
	insertStep(t, db, "s-mid", "mid-exec", 0, run.StepExecuting, "export_data", `{"object":"`+secret+`"}`)

	insertRun(t, db, "bare", run.StateRunning)
	insertRun(t, db, "pending-step", run.StateRunning)
	insertStep(t, db, "s-pending", "pending-step", 0, run.StepPending, "export_data", `{"object":"`+secret+`"}`)
	insertRun(t, db, "error-step", run.StateRunning)
	insertStep(t, db, "s-error", "error-step", 1, run.StepError, "export_data", "")
	insertRun(t, db, "between", run.StateRunning)
	insertStep(t, db, "s-done", "between", 0, run.StepDone, "search_docs", "")
	insertRun(t, db, "created", run.StateCreated)
	insertRun(t, db, "done-run", run.StateCompleted)
	insertRun(t, db, "weird", "paused")

	svc := run.New(db)
	svc.SetExecutor(exec.Exec)
	report, err := svc.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if exec.n() != 0 {
		t.Fatalf("扫描执行了工具 %d 次", exec.n())
	}
	if stateOf(t, db, "awaiting") != run.StateAwaitingApproval {
		t.Fatalf("挂起被改成 %s", stateOf(t, db, "awaiting"))
	}
	if !contains(report.Suspended, "awaiting") {
		t.Fatalf("挂起没有恢复入口 %+v", report.Suspended)
	}
	assertAudit(t, svc, "awaiting", run.AuditApprovalRestored, 1)

	for _, id := range []string{"await-exec", "mid-exec"} {
		if stateOf(t, db, id) != run.StateFailed {
			t.Fatalf("%s 状态 %s", id, stateOf(t, db, id))
		}
		assertAudit(t, svc, id, run.AuditExecutingInterrupted, 1)
		assertNoAudit(t, svc, id, run.AuditApprovalRestored)
	}
	if stepStatus(t, db, "s-mid") != run.StepExecuting {
		t.Fatalf("步骤状态 %s", stepStatus(t, db, "s-mid"))
	}

	for _, id := range []string{"bare", "pending-step", "error-step", "created", "weird"} {
		if stateOf(t, db, id) != run.StateFailed {
			t.Fatalf("%s 状态 %s", id, stateOf(t, db, id))
		}
		assertAudit(t, svc, id, run.AuditIllegalResidual, 1)
	}
	if stateOf(t, db, "between") != run.StateRunning {
		t.Fatalf("断点状态 %s", stateOf(t, db, "between"))
	}
	assertAudit(t, svc, "between", run.AuditBreakpointHeld, 1)
	if stateOf(t, db, "done-run") != run.StateCompleted {
		t.Fatal("已完成的运行被改写")
	}
	if n := len(eventsOf(t, svc, "done-run")); n != 0 {
		t.Fatalf("已完成运行有 %d 条审计", n)
	}
	for _, id := range []string{"awaiting", "mid-exec", "bare", "between"} {
		assertNoSecret(t, eventsOf(t, svc, id))
	}

	if _, err := svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if exec.n() != 0 {
		t.Fatal("第二次扫描执行了工具")
	}
	assertAudit(t, svc, "mid-exec", run.AuditExecutingInterrupted, 1)
	assertAudit(t, svc, "bare", run.AuditIllegalResidual, 1)
	assertAudit(t, svc, "awaiting", run.AuditApprovalRestored, 2)
	assertAudit(t, svc, "between", run.AuditBreakpointHeld, 2)
	if _, err := svc.Resume(ctx, "between"); !errors.Is(err, run.ErrNotAwaiting) || exec.n() != 0 {
		t.Fatalf("断点被 Resume 执行了 %v exec=%d", err, exec.n())
	}
}

func TestOpenMarksExecutingFailedWithoutRerun(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	h, err := workspace.Create(ctx, root, "ws-1")
	if err != nil {
		t.Fatal(err)
	}
	insertRun(t, h.Runs, "run-exec", run.StateRunning)
	svc := run.New(h.Runs)
	exec := &counter{err: errors.New("不该执行")}
	svc.SetExecutor(exec.Exec)
	if err := svc.Begin(ctx, "step-1", "run-exec", "export_data", []byte(`{"object":"`+secret+`"}`)); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := workspace.Open(ctx, root, "ws-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if stateOf(t, reopened.Runs, "run-exec") != run.StateFailed {
		t.Fatalf("重启后状态 %s", stateOf(t, reopened.Runs, "run-exec"))
	}
	if stepStatus(t, reopened.Runs, "step-1") != run.StepExecuting {
		t.Fatalf("步骤 %s", stepStatus(t, reopened.Runs, "step-1"))
	}
	again := run.New(reopened.Runs)
	assertAudit(t, again, "run-exec", run.AuditExecutingInterrupted, 1)
	assertNoSecret(t, eventsOf(t, again, "run-exec"))
	if exec.n() != 0 {
		t.Fatal("重启重跑了工具")
	}

	third, err := workspace.Open(ctx, root, "ws-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = third.Close() })
	_ = reopened.Close()
	assertAudit(t, run.New(third.Runs), "run-exec", run.AuditExecutingInterrupted, 1)
}

func TestResumeFrozenSnapshotOnce(t *testing.T) {
	ctx := t.Context()
	h := openWorkspace(t)
	db := h.Runs
	params := []byte(`{"b":1,"a":"` + secret + `"}`)
	insertRun(t, db, "run-1", run.StateAwaitingApproval)
	ticket := seedTicket(t, db, "run-1", "ticket-1", params, time.Now().Add(time.Hour))
	exec := &counter{}
	svc := run.New(db)
	svc.SetExecutor(exec.Exec)
	if _, err := svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if exec.n() != 0 || stateOf(t, db, "run-1") != run.StateAwaitingApproval {
		t.Fatalf("恢复后就执行了 state=%s n=%d", stateOf(t, db, "run-1"), exec.n())
	}
	assertAudit(t, svc, "run-1", run.AuditApprovalRestored, 1)

	override := []byte(`{"a":"` + secret + `","b":1}`)
	svc.SetApprover(guardApprover{n: exec.n, inner: &approval.Fake{Responses: []approval.Response{{
		Action: approval.ActionApprove,
		Params: override,
	}}}})
	res, err := svc.Resume(ctx, "run-1")
	if err != nil || res.State != run.StateCompleted || !res.Executed || exec.n() != 1 {
		t.Fatalf("resume %+v %v exec=%d", res, err, exec.n())
	}
	if string(exec.args(0)) != string(params) {
		t.Fatalf("执行参数 %s，冻结快照 %s", exec.args(0), params)
	}
	if exec.args(0)[0] == '{' && string(exec.args(0)) == string(override) {
		t.Fatal("执行了批准带回的另一份参数")
	}
	if approvalState(t, db, ticket.ID) != string(approval.StateConsumed) {
		t.Fatalf("确认单 %s", approvalState(t, db, ticket.ID))
	}
	if stepStatus(t, db, ticket.StepID) != run.StepDone {
		t.Fatalf("步骤 %s", stepStatus(t, db, ticket.StepID))
	}
	assertNoSecret(t, eventsOf(t, svc, "run-1"))

	if _, err := svc.Resume(ctx, "run-1"); !errors.Is(err, run.ErrNotAwaiting) || exec.n() != 1 {
		t.Fatalf("第二次 %v exec=%d", err, exec.n())
	}
}

func TestResumeHashMismatchRefusesExecute(t *testing.T) {
	ctx := t.Context()
	h := openWorkspace(t)
	db := h.Runs
	insertRun(t, db, "run-1", run.StateAwaitingApproval)
	ticket := seedTicket(t, db, "run-1", "ticket-1", []byte(`{"object":"`+secret+`"}`), time.Now().Add(time.Hour))
	if _, err := db.ExecContext(ctx, `UPDATE approvals SET params_json = ? WHERE id = ?`, `{"object":"tampered"}`, ticket.ID); err != nil {
		t.Fatal(err)
	}
	exec := &counter{}
	svc := run.New(db)
	svc.SetExecutor(exec.Exec)
	svc.SetApprover(&approval.Fake{Responses: []approval.Response{{Action: approval.ActionApprove}}})
	res, err := svc.Resume(ctx, "run-1")
	if !errors.Is(err, approval.ErrSnapshotMismatch) || res.Executed || res.State != run.StateFailed || exec.n() != 0 {
		t.Fatalf("resume %+v %v exec=%d", res, err, exec.n())
	}
	if approvalState(t, db, ticket.ID) != string(approval.StatePending) {
		t.Fatalf("确认单 %s", approvalState(t, db, ticket.ID))
	}
	assertAudit(t, svc, "run-1", run.AuditHashMismatch, 1)
	assertNoSecret(t, eventsOf(t, svc, "run-1"))
	for _, ev := range eventsOf(t, svc, "run-1") {
		if strings.Contains(ev.Detail, "tampered") {
			t.Fatalf("审计带了参数 %s", ev.Detail)
		}
	}
}

func TestResumeOverrideMismatchRefusesExecute(t *testing.T) {
	ctx := t.Context()
	h := openWorkspace(t)
	db := h.Runs
	insertRun(t, db, "run-1", run.StateAwaitingApproval)
	seedTicket(t, db, "run-1", "ticket-1", []byte(`{"object":"`+secret+`"}`), time.Now().Add(time.Hour))
	exec := &counter{}
	svc := run.New(db)
	svc.SetExecutor(exec.Exec)
	svc.SetApprover(&approval.Fake{Responses: []approval.Response{{
		Action: approval.ActionApprove,
		Params: []byte(`{"object":"other"}`),
	}}})
	res, err := svc.Resume(ctx, "run-1")
	if !errors.Is(err, approval.ErrSnapshotMismatch) || res.Executed || res.State != run.StateFailed || exec.n() != 0 {
		t.Fatalf("resume %+v %v exec=%d", res, err, exec.n())
	}
	assertAudit(t, svc, "run-1", run.AuditHashMismatch, 1)
	assertNoSecret(t, eventsOf(t, svc, "run-1"))
}

func TestResumeDeferAndRejectDoNotExecute(t *testing.T) {
	ctx := t.Context()
	h := openWorkspace(t)
	db := h.Runs
	exec := &counter{}

	insertRun(t, db, "defer", run.StateAwaitingApproval)
	deferred := seedTicket(t, db, "defer", "t-defer", []byte(`{"object":"`+secret+`"}`), time.Now().Add(time.Hour))
	insertRun(t, db, "reject", run.StateAwaitingApproval)
	rejected := seedTicket(t, db, "reject", "t-reject", []byte(`{"object":"`+secret+`"}`), time.Now().Add(time.Hour))

	svc := run.New(db)
	svc.SetExecutor(exec.Exec)
	svc.SetApprover(&approval.Fake{Responses: []approval.Response{{Action: approval.ActionDefer}}})
	res, err := svc.Resume(ctx, "defer")
	if err != nil || res.State != run.StateAwaitingApproval || res.Executed || exec.n() != 0 {
		t.Fatalf("defer %+v %v exec=%d", res, err, exec.n())
	}
	if approvalState(t, db, deferred.ID) != string(approval.StatePending) {
		t.Fatalf("暂缓后确认单 %s", approvalState(t, db, deferred.ID))
	}

	svc.SetApprover(&approval.Fake{Responses: []approval.Response{{Action: approval.ActionReject}}})
	res, err = svc.Resume(ctx, "reject")
	if err != nil || res.State != run.StateRejected || res.Executed || exec.n() != 0 {
		t.Fatalf("reject %+v %v exec=%d", res, err, exec.n())
	}
	if approvalState(t, db, rejected.ID) != string(approval.StateRejected) {
		t.Fatalf("拒绝后确认单 %s", approvalState(t, db, rejected.ID))
	}
}

func TestResumeExpiredAndApprovedDoNotRerun(t *testing.T) {
	ctx := t.Context()
	h := openWorkspace(t)
	db := h.Runs
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	exec := &counter{}

	insertRun(t, db, "old", run.StateAwaitingApproval)
	seedTicket(t, db, "old", "t-old", []byte(`{"object":"`+secret+`"}`), base.Add(time.Minute))
	expired := run.New(db)
	expired.SetClock(func() time.Time { return base.Add(time.Hour) })
	expired.SetExecutor(exec.Exec)
	expired.SetApprover(&approval.Fake{Responses: []approval.Response{{Action: approval.ActionApprove}}})
	res, err := expired.Resume(ctx, "old")
	if !errors.Is(err, approval.ErrExpired) || res.Executed || res.State != run.StateExpired || exec.n() != 0 {
		t.Fatalf("过期 %+v %v exec=%d", res, err, exec.n())
	}

	insertRun(t, db, "pre", run.StateAwaitingApproval)
	pre := seedTicket(t, db, "pre", "t-pre", []byte(`{"object":"`+secret+`"}`), time.Now().Add(time.Hour))
	if _, err := approval.New(db).Decide(ctx, pre.ID, approval.Decision{Action: approval.ActionApprove}); err != nil {
		t.Fatal(err)
	}
	asked := 0
	svc := run.New(db)
	svc.SetExecutor(exec.Exec)
	svc.SetApprover(approvalFunc(func(context.Context, approval.Ticket) (approval.Decision, error) {
		asked++
		return approval.Decision{}, errors.New("已经批准，不该再问")
	}))
	if _, err := svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if exec.n() != 0 || stateOf(t, db, "pre") != run.StateAwaitingApproval {
		t.Fatalf("已批准未消费被提前执行 state=%s n=%d", stateOf(t, db, "pre"), exec.n())
	}
	res, err = svc.Resume(ctx, "pre")
	if err != nil || res.State != run.StateCompleted || !res.Executed || exec.n() != 1 || asked != 0 {
		t.Fatalf("resume %+v %v exec=%d asked=%d", res, err, exec.n(), asked)
	}
}

func TestResumeExecutingDoesNotRerun(t *testing.T) {
	ctx := t.Context()
	h := openWorkspace(t)
	db := h.Runs
	insertRun(t, db, "run-1", run.StateAwaitingApproval)
	insertStep(t, db, "step-1", "run-1", 0, run.StepExecuting, "export_data", `{"object":"`+secret+`"}`)
	seedTicket(t, db, "run-1", "ticket-1", []byte(`{"object":"`+secret+`"}`), time.Now().Add(time.Hour))
	exec := &counter{}
	asked := 0
	svc := run.New(db)
	svc.SetExecutor(exec.Exec)
	svc.SetApprover(approvalFunc(func(context.Context, approval.Ticket) (approval.Decision, error) {
		asked++
		return approval.Decision{Action: approval.ActionApprove}, nil
	}))
	res, err := svc.Resume(ctx, "run-1")
	if !errors.Is(err, run.ErrInterrupted) || res.Executed || res.State != run.StateFailed || exec.n() != 0 || asked != 0 {
		t.Fatalf("resume %+v %v exec=%d asked=%d", res, err, exec.n(), asked)
	}
	assertNoSecret(t, eventsOf(t, svc, "run-1"))
}

func TestResumeExecutorFailureDoesNotRerun(t *testing.T) {
	ctx := t.Context()
	h := openWorkspace(t)
	db := h.Runs
	insertRun(t, db, "run-1", run.StateAwaitingApproval)
	seedTicket(t, db, "run-1", "ticket-1", []byte(`{"object":"`+secret+`"}`), time.Now().Add(time.Hour))
	exec := &counter{err: errors.New("导出失败")}
	svc := run.New(db)
	svc.SetExecutor(exec.Exec)
	svc.SetApprover(&approval.Fake{Responses: []approval.Response{{Action: approval.ActionApprove}}})
	res, err := svc.Resume(ctx, "run-1")
	if err == nil || !res.Executed || res.State != run.StateFailed || exec.n() != 1 {
		t.Fatalf("resume %+v %v exec=%d", res, err, exec.n())
	}
	if stepStatus(t, db, "step-ticket-1") != run.StepError {
		t.Fatalf("步骤 %s", stepStatus(t, db, "step-ticket-1"))
	}
	assertAudit(t, svc, "run-1", run.AuditToolFailed, 1)
	if _, err := svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if exec.n() != 1 || stateOf(t, db, "run-1") != run.StateFailed {
		t.Fatalf("失败后重跑 exec=%d state=%s", exec.n(), stateOf(t, db, "run-1"))
	}
}

type counter struct {
	mu    sync.Mutex
	calls []tools.Call
	err   error
}

func (c *counter) Exec(ctx context.Context, call tools.Call) (tools.Result, error) {
	if err := ctx.Err(); err != nil {
		return tools.Result{}, err
	}
	c.mu.Lock()
	c.calls = append(c.calls, call)
	err := c.err
	c.mu.Unlock()
	if err != nil {
		return tools.Result{}, err
	}
	return tools.Result{Summary: "已导出"}, nil
}

func (c *counter) n() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

func (c *counter) args(i int) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.calls[i].Args...)
}

type guardApprover struct {
	n     func() int
	inner approval.Approver
}

func (g guardApprover) Request(ctx context.Context, ticket approval.Ticket) (approval.Decision, error) {
	if g.n() != 0 {
		return approval.Decision{}, errors.New("确认前工具已执行")
	}
	return g.inner.Request(ctx, ticket)
}

type approvalFunc func(context.Context, approval.Ticket) (approval.Decision, error)

func (f approvalFunc) Request(ctx context.Context, ticket approval.Ticket) (approval.Decision, error) {
	return f(ctx, ticket)
}

func openWorkspace(t *testing.T) *workspace.Handle {
	t.Helper()
	h, err := workspace.Create(t.Context(), t.TempDir(), "ws-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

func insertRun(t *testing.T, db *sql.DB, id, state string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO runs (id, state, version, context, created_at, updated_at)
		VALUES (?, ?, 0, '', ?, ?)`, id, state, now, now); err != nil {
		t.Fatal(err)
	}
}

func insertStep(t *testing.T, db *sql.DB, id, runID string, ordinal int, status, tool, input string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO run_steps (
			id, run_id, ordinal, status, tool_name, input_json, output_json, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, '', ?, ?)`,
		id, runID, ordinal, status, tool, input, now, now); err != nil {
		t.Fatal(err)
	}
}

func seedTicket(t *testing.T, db *sql.DB, runID, ticketID string, params []byte, exp time.Time) approval.Ticket {
	t.Helper()
	hash, err := approval.ParamsHash(params)
	if err != nil {
		t.Fatal(err)
	}
	ticket := approval.Ticket{
		ID:           ticketID,
		RunID:        runID,
		StepID:       "step-" + ticketID,
		WorkspaceID:  "ws-1",
		Action:       "export_data",
		ExportObject: "contacts",
		Params:       append([]byte(nil), params...),
		ParamsHash:   hash,
		ExpiresAt:    exp,
	}
	if err := approval.New(db).Create(t.Context(), ticket); err != nil {
		t.Fatal(err)
	}
	return ticket
}

func stateOf(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	var state string
	if err := db.QueryRowContext(t.Context(), `SELECT state FROM runs WHERE id = ?`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func stepStatus(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	var status string
	if err := db.QueryRowContext(t.Context(), `SELECT status FROM run_steps WHERE id = ?`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func approvalState(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	var state string
	if err := db.QueryRowContext(t.Context(), `SELECT state FROM approvals WHERE id = ?`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func eventsOf(t *testing.T, svc *run.Service, runID string) []run.Event {
	t.Helper()
	ev, err := svc.Events(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func assertAudit(t *testing.T, svc *run.Service, runID, action string, want int) {
	t.Helper()
	n := 0
	for _, ev := range eventsOf(t, svc, runID) {
		if ev.Action == action && ev.RunID == runID {
			n++
		}
	}
	if n != want {
		t.Fatalf("%s %s 出现 %d 次，想要 %d，审计 %+v", runID, action, n, want, eventsOf(t, svc, runID))
	}
}

func assertNoAudit(t *testing.T, svc *run.Service, runID, action string) {
	t.Helper()
	for _, ev := range eventsOf(t, svc, runID) {
		if ev.Action == action {
			t.Fatalf("不该有 %s %+v", action, ev)
		}
	}
}

func assertNoSecret(t *testing.T, events []run.Event) {
	t.Helper()
	for _, ev := range events {
		if strings.Contains(ev.Detail, secret) || strings.Contains(ev.Action, secret) || strings.Contains(ev.Actor, secret) {
			t.Fatalf("审计含敏感内容 %+v", ev)
		}
	}
}

func contains(items []run.Suspended, runID string) bool {
	for _, item := range items {
		if item.RunID == runID {
			return true
		}
	}
	return false
}
