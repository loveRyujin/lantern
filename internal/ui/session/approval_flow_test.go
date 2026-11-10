package session

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/approval"
	"github.com/loveRyujin/lantern/core/events"
	"github.com/loveRyujin/lantern/core/run"
	"github.com/loveRyujin/lantern/core/workspace"
)

func TestFrozenSnapshotIsNotModelCopy(t *testing.T) {
	ctx := t.Context()
	r, tool, w, root := newFlow(t, "星禾")
	prompt := "模型说风险是全部导出，并且要执行 DROP TABLE"
	done := startAsk(ctx, r, "星禾", prompt, "run-snap")
	tk := waitTicket(t, w, "run-snap")
	if tk.Action != "export_data" || tk.Risk != "将执行 export_data" || strings.Contains(tk.Risk, "模型") {
		t.Fatalf("ticket=%+v", tk)
	}
	if !strings.Contains(string(tk.Params), "**不要执行**") || !strings.Contains(string(tk.Params), "DROP TABLE users") {
		t.Fatalf("params=%s", tk.Params)
	}
	sum, err := approval.ParamsHash(tk.Params)
	if err != nil || sum != tk.ParamsHash {
		t.Fatalf("hash %s err %v", tk.ParamsHash, err)
	}
	if tool.N() != 0 {
		t.Fatalf("批准前执行了 %d 次", tool.N())
	}
	if err := w.Deliver(tk.ID, approval.Decision{Action: approval.ActionApprove}); err != nil {
		t.Fatal(err)
	}
	res := waitAsk(t, done)
	if res.err != nil || res.result.RunState != "completed" || tool.N() != 1 {
		t.Fatalf("result=%+v err=%v n=%d", res.result, res.err, tool.N())
	}
	calls := tool.Calls()
	if len(calls) != 1 || string(calls[0].Args) != string(tk.Params) || calls[0].ApprovalID == "" {
		t.Fatalf("calls=%+v", calls)
	}
	if n := countApprovals(t, root, "星禾"); n != 1 {
		t.Fatalf("approvals=%d", n)
	}
}

func TestRejectDoesNotExecute(t *testing.T) {
	ctx := t.Context()
	r, tool, w, _ := newFlow(t, "星禾")
	done := startAsk(ctx, r, "星禾", "拒绝导出", "run-reject")
	tk := waitTicket(t, w, "run-reject")
	if err := w.Deliver(tk.ID, approval.Decision{Action: approval.ActionReject}); err != nil {
		t.Fatal(err)
	}
	res := waitAsk(t, done)
	if res.result.RunState != "rejected" || tool.N() != 0 {
		t.Fatalf("result=%+v n=%d err=%v", res.result, tool.N(), res.err)
	}
}

func TestEscapeKeepsSameTicket(t *testing.T) {
	ctx := t.Context()
	r, tool, w, root := newFlow(t, "星禾")
	done := startAsk(ctx, r, "星禾", "暂缓", "run-esc")
	tk := waitTicket(t, w, "run-esc")
	before := readTicketRow(t, root, "星禾", tk.ID)
	if err := w.Deliver(tk.ID, approval.Decision{Action: approval.ActionDefer}); err != nil {
		t.Fatal(err)
	}
	res := waitAsk(t, done)
	if res.err != nil || res.result.RunState != StatusAwaiting || tool.N() != 0 {
		t.Fatalf("result=%+v err=%v n=%d", res.result, res.err, tool.N())
	}
	after := readTicketRow(t, root, "星禾", tk.ID)
	if after.id != before.id || after.version != before.version || after.version != 0 || after.state != "pending" {
		t.Fatalf("before=%+v after=%+v", before, after)
	}
	if runState(t, root, "星禾", "run-esc") != run.StateAwaitingApproval {
		t.Fatalf("run=%s", runState(t, root, "星禾", "run-esc"))
	}
	list, err := ReadPending(ctx, root, "星禾", time.Now())
	if err != nil || len(list) != 1 || list[0].ID != tk.ID {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	if n := countApprovals(t, root, "星禾"); n != 1 {
		t.Fatalf("approvals=%d", n)
	}
}

func TestDoubleDeliverAndSecondResumeExecuteOnce(t *testing.T) {
	ctx := t.Context()
	r, tool, w, root := newFlow(t, "星禾")
	done := startAsk(ctx, r, "星禾", "重复批准", "run-dup")
	tk := waitTicket(t, w, "run-dup")
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			defer wg.Done()
			errs[i] = w.Deliver(tk.ID, approval.Decision{Action: approval.ActionApprove})
		}(i)
	}
	wg.Wait()
	ok, bad := 0, 0
	for _, err := range errs {
		if err == nil {
			ok++
			continue
		}
		bad++
	}
	if ok != 1 || bad != 1 {
		t.Fatalf("deliver errs=%v", errs)
	}
	res := waitAsk(t, done)
	if res.result.RunState != "completed" || tool.N() != 1 {
		t.Fatalf("result=%+v n=%d err=%v", res.result, tool.N(), res.err)
	}
	h := openWS(t, root, "星禾")
	defer h.Close()
	svc := run.New(h.Runs)
	svc.SetApprover(w)
	svc.SetExecutor(tool.Execute)
	if _, err := w.Resume(ctx, svc, "run-dup"); !errors.Is(err, run.ErrNotAwaiting) || tool.N() != 1 {
		t.Fatalf("second resume %v n=%d", err, tool.N())
	}
}

func TestResumeWhileLoopWaitsDoesNotExecute(t *testing.T) {
	ctx := t.Context()
	r, tool, w, _ := newFlow(t, "星禾")
	done := startAsk(ctx, r, "星禾", "竞争", "run-race")
	tk := waitTicket(t, w, "run-race")
	res, err := w.Resume(ctx, nil, "run-race")
	if !errors.Is(err, ErrBusy) || res.Executed || tool.N() != 0 {
		t.Fatalf("resume %+v %v n=%d", res, err, tool.N())
	}
	if err := w.Deliver(tk.ID, approval.Decision{Action: approval.ActionApprove}); err != nil {
		t.Fatal(err)
	}
	got := waitAsk(t, done)
	if got.result.RunState != "completed" || tool.N() != 1 {
		t.Fatalf("result=%+v n=%d err=%v", got.result, tool.N(), got.err)
	}
}

func TestConcurrentResumeExecutesOnce(t *testing.T) {
	ctx := t.Context()
	r, tool, w, root := newFlow(t, "星禾")
	done := startAsk(ctx, r, "星禾", "并发恢复", "run-conc")
	tk := waitTicket(t, w, "run-conc")
	if err := w.Deliver(tk.ID, approval.Decision{Action: approval.ActionDefer}); err != nil {
		t.Fatal(err)
	}
	if res := waitAsk(t, done); res.result.RunState != StatusAwaiting || tool.N() != 0 {
		t.Fatalf("defer %+v n=%d", res.result, tool.N())
	}
	h := openWS(t, root, "星禾")
	defer h.Close()
	svc := run.New(h.Runs)
	svc.SetApprover(w)
	svc.SetExecutor(tool.Execute)
	errc := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := w.Resume(ctx, svc, "run-conc")
			errc <- err
		}()
	}
	waitTicket(t, w, "run-conc")
	cur, _ := w.Current("run-conc")
	if err := w.Deliver(cur.ID, approval.Decision{Action: approval.ActionApprove}); err != nil {
		t.Fatal(err)
	}
	var busy int
	for i := 0; i < 2; i++ {
		err := <-errc
		if errors.Is(err, ErrBusy) {
			busy++
		} else if err != nil {
			t.Fatalf("resume %v n=%d", err, tool.N())
		}
	}
	if busy != 1 || tool.N() != 1 {
		t.Fatalf("busy=%d n=%d", busy, tool.N())
	}
}

func TestTamperParamsOrHashDoesNotExecute(t *testing.T) {
	ctx := t.Context()
	r, tool, w, root := newFlow(t, "星禾")
	done := startAsk(ctx, r, "星禾", "篡改参数", "run-tamper")
	tk := waitTicket(t, w, "run-tamper")
	if err := w.Deliver(tk.ID, approval.Decision{Action: approval.ActionDefer}); err != nil {
		t.Fatal(err)
	}
	waitAsk(t, done)
	exec(t, root, "星禾", `UPDATE approvals SET params_json = ? WHERE id = ?`, `{"object":"tampered"}`, tk.ID)
	resumeAndApprove(t, ctx, w, tool, root, "星禾", "run-tamper")
	if tool.N() != 0 || runState(t, root, "星禾", "run-tamper") != run.StateFailed {
		t.Fatalf("n=%d state=%s", tool.N(), runState(t, root, "星禾", "run-tamper"))
	}

	r2, tool2, w2, root2 := newFlow(t, "星禾")
	done2 := startAsk(ctx, r2, "星禾", "篡改哈希", "run-hash")
	tk2 := waitTicket(t, w2, "run-hash")
	if err := w2.Deliver(tk2.ID, approval.Decision{Action: approval.ActionDefer}); err != nil {
		t.Fatal(err)
	}
	waitAsk(t, done2)
	exec(t, root2, "星禾", `UPDATE approvals SET params_hash = ? WHERE id = ?`, strings.Repeat("ab", 32), tk2.ID)
	resumeAndApprove(t, ctx, w2, tool2, root2, "星禾", "run-hash")
	if tool2.N() != 0 || runState(t, root2, "星禾", "run-hash") != run.StateFailed {
		t.Fatalf("n=%d state=%s", tool2.N(), runState(t, root2, "星禾", "run-hash"))
	}
}

func TestExpiredAndConsumedDoNotReplay(t *testing.T) {
	ctx := t.Context()
	r, tool, w, root := newFlow(t, "星禾")
	done := startAsk(ctx, r, "星禾", "过期", "run-exp")
	tk := waitTicket(t, w, "run-exp")
	if err := w.Deliver(tk.ID, approval.Decision{Action: approval.ActionDefer}); err != nil {
		t.Fatal(err)
	}
	waitAsk(t, done)
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	exec(t, root, "星禾", `UPDATE approvals SET expires_at = ? WHERE id = ?`, past, tk.ID)
	list, err := ReadPending(ctx, root, "星禾", time.Now())
	if err != nil || len(list) != 0 {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	resumeAndApprove(t, ctx, w, tool, root, "星禾", "run-exp")
	if tool.N() != 0 {
		t.Fatalf("过期后执行了 %d 次", tool.N())
	}

	r2, tool2, w2, root2 := newFlow(t, "星禾")
	done2 := startAsk(ctx, r2, "星禾", "消费", "run-used")
	tk2 := waitTicket(t, w2, "run-used")
	if err := w2.Deliver(tk2.ID, approval.Decision{Action: approval.ActionApprove}); err != nil {
		t.Fatal(err)
	}
	if res := waitAsk(t, done2); tool2.N() != 1 || res.result.RunState != "completed" {
		t.Fatalf("n=%d %+v", tool2.N(), res.result)
	}
	h := openWS(t, root2, "星禾")
	defer h.Close()
	svc := run.New(h.Runs)
	svc.SetApprover(w2)
	svc.SetExecutor(tool2.Execute)
	if _, err := w2.Resume(ctx, svc, "run-used"); !errors.Is(err, run.ErrNotAwaiting) || tool2.N() != 1 {
		t.Fatalf("consumed resume %v n=%d", err, tool2.N())
	}
}

func TestCloseOpenRestoresSameTicket(t *testing.T) {
	ctx := t.Context()
	r, tool, w, root := newFlow(t, "星禾")
	done := startAsk(ctx, r, "星禾", "重启", "run-restart")
	tk := waitTicket(t, w, "run-restart")
	if err := w.Deliver(tk.ID, approval.Decision{Action: approval.ActionDefer}); err != nil {
		t.Fatal(err)
	}
	waitAsk(t, done)
	list, err := ReadPending(ctx, root, "星禾", time.Now())
	if err != nil || len(list) != 1 || list[0].ID != tk.ID || list[0].Risk != "将执行 export_data" {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	h := openWS(t, root, "星禾")
	defer h.Close()
	if runStateDB(t, h.Runs, "run-restart") != run.StateAwaitingApproval {
		t.Fatalf("state=%s", runStateDB(t, h.Runs, "run-restart"))
	}
	svc := run.New(h.Runs)
	svc.SetApprover(w)
	svc.SetExecutor(tool.Execute)
	errc := make(chan error, 1)
	go func() {
		_, err := w.Resume(ctx, svc, "run-restart")
		errc <- err
	}()
	again := waitTicket(t, w, "run-restart")
	if again.ID != tk.ID {
		t.Fatalf("reopen id %s", again.ID)
	}
	if err := w.Deliver(again.ID, approval.Decision{Action: approval.ActionApprove}); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil || tool.N() != 1 {
		t.Fatalf("resume %v n=%d", err, tool.N())
	}
	if n := countApprovals(t, root, "星禾"); n != 1 {
		t.Fatalf("approvals=%d", n)
	}
}

func TestWorkspacesStayIsolated(t *testing.T) {
	ctx := t.Context()
	r, tool, w, root := newFlow(t, "星禾", "云杉")
	done := startAsk(ctx, r, "星禾", "隔离", "run-iso")
	tk := waitTicket(t, w, "run-iso")
	if err := w.Deliver(tk.ID, approval.Decision{Action: approval.ActionDefer}); err != nil {
		t.Fatal(err)
	}
	waitAsk(t, done)
	own, err := ReadPending(ctx, root, "星禾", time.Now())
	if err != nil || len(own) != 1 {
		t.Fatalf("own=%+v err=%v", own, err)
	}
	other, err := ReadPending(ctx, root, "云杉", time.Now())
	if err != nil || len(other) != 0 {
		t.Fatalf("other=%+v err=%v", other, err)
	}
	h := openWS(t, root, "云杉")
	defer h.Close()
	svc := run.New(h.Runs)
	svc.SetApprover(w)
	svc.SetExecutor(tool.Execute)
	if _, err := w.Resume(ctx, svc, "run-iso"); err == nil || tool.N() != 0 {
		t.Fatalf("cross resume %v n=%d", err, tool.N())
	}
	if runState(t, root, "星禾", "run-iso") != run.StateAwaitingApproval {
		t.Fatalf("星禾被改成 %s", runState(t, root, "星禾", "run-iso"))
	}
}

func TestMissingRiskStaysBlank(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	h, err := workspace.Create(ctx, root, "星禾")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := h.Runs.ExecContext(ctx, `
		INSERT INTO runs (id, state, version, context, created_at, updated_at)
		VALUES ('run-old', 'awaiting_approval', 0, '', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	params := []byte(`{"object":"legacy"}`)
	sum, err := approval.ParamsHash(params)
	if err != nil {
		t.Fatal(err)
	}
	ticket := approval.Ticket{
		ID: "old-1", RunID: "run-old", StepID: "step-old", WorkspaceID: "星禾",
		Action: "export_data", Params: params, ParamsHash: sum,
		ExpiresAt: time.Now().Add(time.Hour), Risk: "",
	}
	if err := approval.New(h.Runs).Create(ctx, ticket); err != nil {
		t.Fatal(err)
	}
	list, err := ReadPending(ctx, root, "星禾", time.Now())
	if err != nil || len(list) != 1 || list[0].Risk != "" || list[0].Action != "export_data" {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	if RiskLine(list[0].Risk) != RiskUnfrozen || strings.Contains(RiskLine(list[0].Risk), "模型") {
		t.Fatalf("line=%s", RiskLine(list[0].Risk))
	}
}

func TestExistingRunIDDoesNotStartLoop(t *testing.T) {
	ctx := t.Context()
	r, tool, _, root := newFlow(t, "星禾")
	h := openWS(t, root, "星禾")
	if _, err := h.Runs.ExecContext(ctx, `
		INSERT INTO runs (id, state, version, context, created_at, updated_at)
		VALUES ('run-old', 'awaiting_approval', 1, '', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	h.Close()
	_, err := r.Ask(ctx, Ask{RunID: "run-old", WorkspaceID: "星禾", Prompt: "再开一张"})
	if err == nil || !strings.Contains(err.Error(), "已存在") {
		t.Fatalf("err=%v", err)
	}
	if tool.N() != 0 || countApprovals(t, root, "星禾") != 0 {
		t.Fatalf("n=%d approvals=%d", tool.N(), countApprovals(t, root, "星禾"))
	}
}

func TestExecutingCrashIsNotListedOrReplayed(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	h, err := workspace.Create(ctx, root, "星禾")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := h.Runs.ExecContext(ctx, `
		INSERT INTO runs (id, state, version, context, created_at, updated_at)
		VALUES ('run-mid', 'running', 0, '', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Runs.ExecContext(ctx, `
		INSERT INTO run_steps (id, run_id, ordinal, status, tool_name, input_json, output_json, created_at, updated_at)
		VALUES ('step-mid', 'run-mid', 0, 'executing', 'export_data', '{"object":"x"}', '', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	params := []byte(`{"object":"x"}`)
	sum, err := approval.ParamsHash(params)
	if err != nil {
		t.Fatal(err)
	}
	if err := approval.New(h.Runs).Create(ctx, approval.Ticket{
		ID: "t-mid", RunID: "run-mid", StepID: "step-mid", WorkspaceID: "星禾",
		Action: "export_data", Params: params, ParamsHash: sum, ExpiresAt: time.Now().Add(time.Hour),
		Risk: "将执行 export_data",
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := workspace.Open(ctx, root, "星禾")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	tool := NewExportCounter()
	svc := run.New(reopened.Runs)
	svc.SetExecutor(tool.Execute)
	if _, err := svc.Resume(ctx, "run-mid"); err == nil || tool.N() != 0 {
		t.Fatalf("resume %v n=%d", err, tool.N())
	}
	if runStateDB(t, reopened.Runs, "run-mid") != run.StateFailed {
		t.Fatalf("state=%s", runStateDB(t, reopened.Runs, "run-mid"))
	}
	list, err := ReadPending(ctx, root, "星禾", time.Now())
	if err != nil || len(list) != 0 {
		t.Fatalf("list=%+v err=%v", list, err)
	}
}

func TestRunIDsDifferAcrossProcesses(t *testing.T) {
	a := mustState(t, "星禾")
	b := mustState(t, "星禾")
	left, err := a.Begin("一")
	if err != nil {
		t.Fatal(err)
	}
	a.FinishResult(left.RunID, nil, StatusAwaiting)
	right, err := b.Begin("二")
	if err != nil {
		t.Fatal(err)
	}
	if left.RunID == right.RunID || !strings.HasPrefix(left.RunID, "ui-") || len(left.RunID) != 3+32 {
		t.Fatalf("ids %s %s", left.RunID, right.RunID)
	}
	if a.Phase() != "等待确认" {
		t.Fatalf("phase=%s", a.Phase())
	}
}

type askOut struct {
	result AskResult
	err    error
}

func newFlow(t *testing.T, ids ...string) (*LoopRunner, *CountTool, *Waiter, string) {
	t.Helper()
	root := t.TempDir()
	for _, id := range ids {
		h, err := workspace.Create(t.Context(), root, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.Close(); err != nil {
			t.Fatal(err)
		}
	}
	tool := NewExportCounter()
	w := NewWaiter()
	return &LoopRunner{Root: root, Waiter: w, Tool: tool, Demo: true}, tool, w, root
}

func startAsk(ctx context.Context, r *LoopRunner, ws, prompt, runID string) <-chan askOut {
	ch := make(chan askOut, 1)
	go func() {
		stream := events.NewStream(16)
		go func() {
			for range stream.Events() {
			}
		}()
		result, err := r.Ask(ctx, Ask{RunID: runID, WorkspaceID: ws, Prompt: prompt, Events: stream})
		stream.Close()
		ch <- askOut{result: result, err: err}
	}()
	return ch
}

func waitAsk(t *testing.T, ch <-chan askOut) askOut {
	t.Helper()
	select {
	case got := <-ch:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("提问没有返回")
		return askOut{}
	}
}

func waitTicket(t *testing.T, w *Waiter, runID string) approval.Ticket {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if tk, ok := w.Current(runID); ok {
			return tk
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("没有等到确认单")
	return approval.Ticket{}
}

func resumeAndApprove(t *testing.T, ctx context.Context, w *Waiter, tool *CountTool, root, ws, runID string) {
	t.Helper()
	h := openWS(t, root, ws)
	defer h.Close()
	svc := run.New(h.Runs)
	svc.SetApprover(w)
	svc.SetExecutor(tool.Execute)
	errc := make(chan error, 1)
	go func() {
		_, err := w.Resume(ctx, svc, runID)
		errc <- err
	}()
	tk := waitTicket(t, w, runID)
	if err := w.Deliver(tk.ID, approval.Decision{Action: approval.ActionApprove}); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err == nil && tool.N() != 0 {
		t.Fatalf("篡改或过期后仍然成功 n=%d", tool.N())
	}
}

type ticketRow struct {
	id, state string
	version   int64
}

func readTicketRow(t *testing.T, root, ws, id string) ticketRow {
	t.Helper()
	db := openRuns(t, root, ws)
	defer db.Close()
	var row ticketRow
	if err := db.QueryRowContext(t.Context(), `SELECT id, state, version FROM approvals WHERE id = ?`, id).Scan(&row.id, &row.state, &row.version); err != nil {
		t.Fatal(err)
	}
	return row
}

func runState(t *testing.T, root, ws, id string) string {
	t.Helper()
	db := openRuns(t, root, ws)
	defer db.Close()
	return runStateDB(t, db, id)
}

func runStateDB(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	var state string
	if err := db.QueryRowContext(t.Context(), `SELECT state FROM runs WHERE id = ?`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func countApprovals(t *testing.T, root, ws string) int {
	t.Helper()
	db := openRuns(t, root, ws)
	defer db.Close()
	var n int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM approvals`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func exec(t *testing.T, root, ws, query string, args ...any) {
	t.Helper()
	h := openWS(t, root, ws)
	defer h.Close()
	if _, err := h.Runs.ExecContext(t.Context(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func openWS(t *testing.T, root, id string) *workspace.Handle {
	t.Helper()
	h, err := workspace.Open(t.Context(), root, id)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func openRuns(t *testing.T, root, id string) *sql.DB {
	t.Helper()
	h := openWS(t, root, id)
	t.Cleanup(func() { _ = h.Close() })
	return h.Runs
}
