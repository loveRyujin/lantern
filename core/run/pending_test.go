package run_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/approval"
	"github.com/loveRyujin/lantern/core/run"
)

func TestListPendingReadsRiskWithoutWriting(t *testing.T) {
	ctx := t.Context()
	h := openWorkspace(t)
	db := h.Runs
	now := time.Now().UTC()
	insertRun(t, db, "live", run.StateAwaitingApproval)
	ticket := approval.Ticket{
		ID: "t-live", RunID: "live", StepID: "step-live", WorkspaceID: "ws-1",
		Action: "export_data", ExportObject: "contacts",
		Params: []byte(`{"object":"**不要执行**"}`), ExpiresAt: now.Add(time.Hour),
		Risk: "将执行 export_data",
	}
	sum, err := approval.ParamsHash(ticket.Params)
	if err != nil {
		t.Fatal(err)
	}
	ticket.ParamsHash = sum
	if err := approval.New(db).Create(ctx, ticket); err != nil {
		t.Fatal(err)
	}
	before := auditCount(t, db)

	got, err := run.ListPending(ctx, db, "ws-1", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "t-live" || got[0].Risk != "将执行 export_data" || got[0].Action != "export_data" || string(got[0].Params) != string(ticket.Params) || got[0].ParamsHash != sum {
		t.Fatalf("pending=%+v", got)
	}
	if auditCount(t, db) != before {
		t.Fatalf("只读查询写了审计 %d → %d", before, auditCount(t, db))
	}

	other, err := run.ListPending(ctx, db, "ws-2", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Fatalf("别的工作区标识看见了票据 %+v", other)
	}
}

func TestListPendingSkipsExpiredConsumedAndExecuting(t *testing.T) {
	ctx := t.Context()
	h := openWorkspace(t)
	db := h.Runs
	now := time.Now().UTC()
	machine := approval.New(db)

	insertRun(t, db, "old", run.StateAwaitingApproval)
	old := mustPending(t, machine, "old", "t-old", now.Add(time.Hour), "")
	if _, err := db.ExecContext(ctx, `UPDATE approvals SET expires_at = ? WHERE id = ?`, now.Add(-time.Minute).Format(time.RFC3339Nano), old.ID); err != nil {
		t.Fatal(err)
	}

	insertRun(t, db, "used", run.StateAwaitingApproval)
	used := mustPending(t, machine, "used", "t-used", now.Add(time.Hour), "将执行 export_data")
	if _, err := machine.Decide(ctx, used.ID, approval.Decision{Action: approval.ActionApprove}); err != nil {
		t.Fatal(err)
	}
	if err := machine.Consume(ctx, used.ID, approval.ConsumeContext{RunID: used.RunID, StepID: used.StepID, ArgsHash: used.ParamsHash}); err != nil {
		t.Fatal(err)
	}

	insertRun(t, db, "mid", run.StateAwaitingApproval)
	mustPending(t, machine, "mid", "t-mid", now.Add(time.Hour), "将执行 export_data")
	insertStep(t, db, "s-mid", "mid", 0, run.StepExecuting, "export_data", `{"object":"x"}`)

	insertRun(t, db, "blank", run.StateAwaitingApproval)
	blank := mustPending(t, machine, "blank", "t-blank", now.Add(time.Hour), "")

	got, err := run.ListPending(ctx, db, "ws-1", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != blank.ID || got[0].Risk != "" {
		t.Fatalf("pending=%+v", got)
	}
}

func mustPending(t *testing.T, m *approval.Machine, runID, id string, exp time.Time, risk string) approval.Ticket {
	t.Helper()
	params := []byte(`{"object":"contacts"}`)
	sum, err := approval.ParamsHash(params)
	if err != nil {
		t.Fatal(err)
	}
	ticket := approval.Ticket{
		ID: id, RunID: runID, StepID: "step-" + id, WorkspaceID: "ws-1",
		Action: "export_data", ExportObject: "contacts", Params: params, ParamsHash: sum,
		ExpiresAt: exp, Risk: risk,
	}
	if err := m.Create(t.Context(), ticket); err != nil {
		t.Fatal(err)
	}
	return ticket
}

func auditCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM audit`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
