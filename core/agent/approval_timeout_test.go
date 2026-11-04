package agent

import (
	"context"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/approval"
	"github.com/loveRyujin/lantern/core/llm"
	"github.com/loveRyujin/lantern/core/run"
	"github.com/loveRyujin/lantern/core/tools"
)

func TestSilentApproverExpiresTicketWithoutExecution(t *testing.T) {
	ws := openRuns(t)
	h := newHarness(t, tools.SpecExportData, llm.CallTools(llm.ToolCall{
		ID: "e", Name: "export_data", Arguments: `{"object":"contacts","fields":["phone"]}`,
	}))
	h.loop.Tickets = approval.New(ws.Runs)
	status, steps := Persist(run.New(ws.Runs))
	h.loop.Status = status
	h.loop.Steps = steps
	h.loop.Config.ApprovalTTL = 40 * time.Millisecond
	h.loop.Config.TotalTimeout = 2 * time.Second
	h.approver.Responses = []approval.Response{{Wait: true}}

	out, err := h.run()
	if out.State != StateExpired || errorCode(err) != "approval_expired" || h.echo.n() != 0 {
		t.Fatalf("超时未响应应过期且不执行: %+v %v exec=%d", out, err, h.echo.n())
	}
	var runState, ticketState string
	if err := ws.Runs.QueryRowContext(t.Context(), `SELECT state FROM runs WHERE id = ?`, "run-1").Scan(&runState); err != nil {
		t.Fatal(err)
	}
	if err := ws.Runs.QueryRowContext(t.Context(), `SELECT state FROM approvals`).Scan(&ticketState); err != nil {
		t.Fatal(err)
	}
	if runState != StateExpired || ticketState != string(approval.StateExpired) || len(h.approver.Tickets()) != 1 {
		t.Fatalf("run=%s ticket=%s approver=%d", runState, ticketState, len(h.approver.Tickets()))
	}
}

func TestParentCancelDuringApprovalDoesNotExpire(t *testing.T) {
	ws := openRuns(t)
	h := newHarness(t, tools.SpecExportData, llm.CallTools(llm.ToolCall{
		ID: "e", Name: "export_data", Arguments: `{"object":"contacts"}`,
	}))
	h.loop.Tickets = approval.New(ws.Runs)
	status, steps := Persist(run.New(ws.Runs))
	h.loop.Status = status
	h.loop.Steps = steps
	h.loop.Config.ApprovalTTL = time.Hour
	started := make(chan struct{})
	h.loop.Approver = cancelWait{started: started}

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-started
		cancel()
	}()
	out, err := h.loop.Run(ctx, Input{RunID: "run-1", WorkspaceID: "ws-1", Prompt: "导出"})
	if out.State != StateCancelled || errorCode(err) != "cancelled" || h.echo.n() != 0 {
		t.Fatalf("父上下文取消不应记成过期: %+v %v exec=%d", out, err, h.echo.n())
	}
	var ticketState string
	if err := ws.Runs.QueryRowContext(t.Context(), `SELECT state FROM approvals`).Scan(&ticketState); err != nil {
		t.Fatal(err)
	}
	if ticketState != string(approval.StatePending) {
		t.Fatalf("取消后确认单 %s", ticketState)
	}
}

type cancelWait struct {
	started chan struct{}
}

func (c cancelWait) Request(ctx context.Context, ticket approval.Ticket) (approval.Decision, error) {
	if err := ticket.Validate(); err != nil {
		return approval.Decision{}, err
	}
	close(c.started)
	<-ctx.Done()
	return approval.Decision{}, ctx.Err()
}
