package run_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/approval"
	"github.com/loveRyujin/lantern/core/run"
	"github.com/loveRyujin/lantern/core/workspace"
)

var terminalStates = []string{
	run.StateCompleted, run.StateFailed, run.StateCancelled, run.StateRejected, run.StateExpired,
}

var allStates = []string{
	run.StateCreated, run.StateRunning, run.StateAwaitingApproval,
	run.StateCompleted, run.StateFailed, run.StateCancelled, run.StateRejected, run.StateExpired,
}

func TestTerminalStateIsNotRewritten(t *testing.T) {
	ctx := t.Context()
	h := openWorkspace(t)
	db := h.Runs
	svc := run.New(db)
	write := map[string]func(id, to string) error{
		"Set":        func(id, to string) error { return svc.Set(ctx, id, to) },
		"forceState": func(id, to string) error { return svc.ForceState(ctx, id, to) },
	}
	for name, fn := range write {
		for _, from := range terminalStates {
			for _, to := range allStates {
				id := name + "-" + from + "-" + to
				insertRun(t, db, id, from)
				beforeVer := versionOf(t, db, id)
				beforeTr := transitionsOf(t, db, id)
				err := fn(id, to)
				if to == from {
					if err != nil {
						t.Errorf("%s %s->%s 同一终态应是空操作：%v", name, from, to, err)
					}
				} else {
					var te *run.TransitionError
					if !errors.Is(err, run.ErrTerminal) || !errors.As(err, &te) || te.From != from || te.To != to {
						t.Errorf("%s %s->%s err=%v，想要 ErrTerminal", name, from, to, err)
					}
				}
				if got := stateOf(t, db, id); got != from {
					t.Errorf("%s %s->%s 之后 state=%s", name, from, to, got)
				}
				if got := versionOf(t, db, id); got != beforeVer {
					t.Errorf("%s %s->%s 之后 version %d→%d", name, from, to, beforeVer, got)
				}
				if got := transitionsOf(t, db, id); strings.Join(got, "|") != strings.Join(beforeTr, "|") {
					t.Errorf("%s %s->%s 追加了迁移 %v", name, from, to, got)
				}
			}
		}
	}
}

func TestSetKeepsLegalTransitions(t *testing.T) {
	ctx := t.Context()
	h := openWorkspace(t)
	db := h.Runs
	svc := run.New(db)
	insertRun(t, db, "run-1", run.StateCreated)
	for _, to := range []string{run.StateRunning, run.StateAwaitingApproval, run.StateRunning, run.StateCompleted} {
		if err := svc.Set(ctx, "run-1", to); err != nil {
			t.Fatalf("Set %s: %v", to, err)
		}
	}
	want := "->created|created->running|running->awaiting_approval|awaiting_approval->running|running->completed"
	if got := strings.Join(transitionsOf(t, db, "run-1"), "|"); got != want {
		t.Fatalf("迁移 %s", got)
	}
	if err := svc.Set(ctx, "missing", run.StateRunning); !errors.Is(err, run.ErrNotFound) {
		t.Fatalf("不存在的运行 %v", err)
	}
}

// Recover 跳过 Hold 登记的运行；release 之后同一份残留照常按 FR-SM-5 归类。
func TestRecoverSkipsHeldRunUntilReleased(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	h, err := workspace.Create(ctx, root, "ws-1")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	db := h.Runs
	insertRun(t, db, "run-exec", run.StateRunning)
	insertStep(t, db, "step-1", "run-exec", 0, run.StepExecuting, "export_data", `{}`)
	insertRun(t, db, "run-left", run.StateRunning)
	insertStep(t, db, "step-2", "run-left", 0, run.StepExecuting, "export_data", `{}`)

	release, err := run.Hold(ctx, db, "run-exec")
	if err != nil {
		t.Fatal(err)
	}
	// 另开一个连接池，等同 Ask 里的 workspace.Open：登记按库文件识别，不按 *sql.DB。
	o, err := workspace.Open(ctx, root, "ws-1")
	if err != nil {
		t.Fatal(err)
	}
	_ = o.Close()
	if got := stateOf(t, db, "run-exec"); got != run.StateRunning {
		t.Fatalf("活跃运行被改成 %s", got)
	}
	if got := stateOf(t, db, "run-left"); got != run.StateFailed {
		t.Fatalf("没登记的残留 state=%s", got)
	}
	svc := run.New(db)
	rep, err := svc.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(rep.Active, ",") != "run-exec" || len(rep.Interrupted) != 0 {
		t.Fatalf("report %+v", rep)
	}
	assertNoAudit(t, svc, "run-exec", run.AuditExecutingInterrupted)

	other, err := workspace.Create(ctx, root, "ws-2")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	insertRun(t, other.Runs, "run-exec", run.StateRunning)
	insertStep(t, other.Runs, "step-1", "run-exec", 0, run.StepExecuting, "export_data", `{}`)
	if rep, err := run.New(other.Runs).Recover(ctx); err != nil || strings.Join(rep.Interrupted, ",") != "run-exec" {
		t.Fatalf("另一个工作区的同名运行不该被跳过 %+v %v", rep, err)
	}

	release()
	release()
	rep, err = svc.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(rep.Interrupted, ",") != "run-exec" || len(rep.Active) != 0 {
		t.Fatalf("release 之后 report %+v", rep)
	}
	assertAudit(t, svc, "run-exec", run.AuditExecutingInterrupted, 1)
}

// 步骤写入失败（这里用触发器注入）时，批准、消费和运行迁移一起回滚：
// 票据仍是 pending、可以重开，运行仍挂起，工具没执行。去掉故障后重开，恰好执行一次。
func TestResumeBeginFailureRollsBackConsume(t *testing.T) {
	ctx := t.Context()
	h := openWorkspace(t)
	db := h.Runs
	insertRun(t, db, "run-1", run.StateAwaitingApproval)
	ticket := seedTicket(t, db, "run-1", "ticket-1", []byte(`{"a":1}`), time.Now().Add(time.Hour))
	if _, err := db.ExecContext(ctx, `
		CREATE TRIGGER inject_begin_fail BEFORE INSERT ON run_steps
		BEGIN SELECT RAISE(ABORT, 'injected begin failure'); END`); err != nil {
		t.Fatal(err)
	}
	exec := &counter{}
	svc := run.New(db)
	svc.SetExecutor(exec.Exec)
	svc.SetApprover(&approval.Fake{Responses: []approval.Response{{Action: approval.ActionApprove}, {Action: approval.ActionApprove}}})

	res, err := svc.Resume(ctx, "run-1")
	if err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("想要注入的错误，得到 %v", err)
	}
	if res.State != run.StateAwaitingApproval || res.Executed || exec.n() != 0 {
		t.Fatalf("res=%+v exec=%d", res, exec.n())
	}
	assertRetryable(t, db, "run-1", ticket.ID)

	if _, err := db.ExecContext(ctx, `DROP TRIGGER inject_begin_fail`); err != nil {
		t.Fatal(err)
	}
	res, err = svc.Resume(ctx, "run-1")
	if err != nil || res.State != run.StateCompleted || exec.n() != 1 {
		t.Fatalf("重开 res=%+v err=%v exec=%d", res, err, exec.n())
	}
	assertAfterApproval(t, db, "run-1", "awaiting_approval->running|running->completed")
	if approvalState(t, db, ticket.ID) != string(approval.StateConsumed) {
		t.Fatalf("确认单 %s", approvalState(t, db, ticket.ID))
	}
}

// 写锁一直被别的连接占着：有界重试之后返回 ErrBusy，什么都没提交。
// 锁只占一会儿：等到锁之后正常执行一次。
func TestResumeBusyIsRetryableNotConsumed(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	h, err := workspace.Create(ctx, root, "ws-1")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	db := h.Runs
	insertRun(t, db, "run-1", run.StateAwaitingApproval)
	ticket := seedTicket(t, db, "run-1", "ticket-1", []byte(`{"a":1}`), time.Now().Add(time.Hour))

	// 单连接 + 短 busy_timeout，免得每次等满 DSN 里的 5 秒。
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout = 50`); err != nil {
		t.Fatal(err)
	}
	defer run.SetBusyBudget(300 * time.Millisecond)()

	locker, err := workspace.OpenPrepared(ctx, root, "ws-1")
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close()
	lock := func() (unlock func()) {
		conn, err := locker.Runs.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
			t.Fatal(err)
		}
		return func() {
			_, _ = conn.ExecContext(ctx, `ROLLBACK`)
			_ = conn.Close()
		}
	}

	exec := &counter{}
	svc := run.New(db)
	svc.SetExecutor(exec.Exec)
	svc.SetApprover(&approval.Fake{Responses: []approval.Response{{Action: approval.ActionApprove}, {Action: approval.ActionApprove}}})

	unlock := lock()
	started := time.Now()
	res, err := svc.Resume(ctx, "run-1")
	elapsed := time.Since(started)
	unlock()
	if !errors.Is(err, run.ErrBusy) {
		t.Fatalf("想要 ErrBusy，得到 %v", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("重试没有界：%v", elapsed)
	}
	if res.State != run.StateAwaitingApproval || res.Executed || exec.n() != 0 {
		t.Fatalf("res=%+v exec=%d", res, exec.n())
	}
	assertRetryable(t, db, "run-1", ticket.ID)

	unlock = lock()
	go func() {
		time.Sleep(150 * time.Millisecond)
		unlock()
	}()
	res, err = svc.Resume(ctx, "run-1")
	if err != nil || res.State != run.StateCompleted || exec.n() != 1 {
		t.Fatalf("锁释放后 res=%+v err=%v exec=%d", res, err, exec.n())
	}
	assertAfterApproval(t, db, "run-1", "awaiting_approval->running|running->completed")
}

func assertRetryable(t *testing.T, db *sql.DB, runID, ticketID string) {
	t.Helper()
	if got := stateOf(t, db, runID); got != run.StateAwaitingApproval {
		t.Fatalf("运行 %s", got)
	}
	if got := approvalState(t, db, ticketID); got != string(approval.StatePending) {
		t.Fatalf("确认单 %s，想要 pending", got)
	}
	var steps int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM run_steps WHERE run_id = ?`, runID).Scan(&steps); err != nil {
		t.Fatal(err)
	}
	if steps != 0 {
		t.Fatalf("留下了 %d 个步骤", steps)
	}
	assertAfterApproval(t, db, runID, "")
	pending, err := run.ListPending(t.Context(), db, "ws-1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != ticketID {
		t.Fatalf("待确认列表 %+v，票据应当可以重开", pending)
	}
}

// assertAfterApproval 比较挂起之后的迁移（去掉 ->awaiting_approval 那一条）。
func assertAfterApproval(t *testing.T, db *sql.DB, runID, want string) {
	t.Helper()
	trs := transitionsOf(t, db, runID)
	if len(trs) == 0 || trs[0] != "->awaiting_approval" {
		t.Fatalf("迁移 %v", trs)
	}
	if got := strings.Join(trs[1:], "|"); got != want {
		t.Fatalf("批准后的迁移 %q，想要 %q", got, want)
	}
}

func versionOf(t *testing.T, db *sql.DB, id string) int64 {
	t.Helper()
	var v int64
	if err := db.QueryRowContext(t.Context(), `SELECT version FROM runs WHERE id = ?`, id).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func transitionsOf(t *testing.T, db *sql.DB, id string) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `
		SELECT from_state || '->' || to_state FROM run_transitions WHERE run_id = ? ORDER BY id`, id)
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
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
