package agent

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/approval"
	"github.com/loveRyujin/lantern/core/llm"
	"github.com/loveRyujin/lantern/core/policy"
	"github.com/loveRyujin/lantern/core/tools"
)

func TestTruncatedFinalAnswerIsRetryableAndNotAToolFailure(t *testing.T) {
	h := newHarness(t, echoSpec(),
		llm.Step{FinishReason: llm.FinishLength, Content: "半截回答"},
		llm.Say("不该走到这里"),
	)
	out, err := h.run()
	if out.ModelCalls != 1 || out.State != StateFailed || !retryable(err) || errorCode(err) != CodeAnswerIncomplete {
		t.Fatalf("%+v %v", out, err)
	}
	if h.echo.n() != 0 || strings.Contains(out.Text, "不该走到这里") {
		t.Fatalf("截断的最终回答仍继续了: exec=%d text=%q", h.echo.n(), out.Text)
	}
	for _, ev := range h.log.Events() {
		if ev.Kind != "" {
			t.Fatalf("最终回答截断不应记成工具失败: %+v", ev)
		}
	}
}

func TestTruncatedAnswerDoesNotConsumeArgumentBudget(t *testing.T) {
	h := newHarness(t, echoSpec(),
		badStep("b1"),
		badStep("b2"),
		llm.Step{FinishReason: llm.FinishLength, Content: "半截"},
		llm.Say("不该走到这里"),
	)
	out, err := h.run()
	if out.ModelCalls != 3 || out.State != StateFailed || !retryable(err) || errorCode(err) != CodeAnswerIncomplete {
		t.Fatalf("%+v %v", out, err)
	}
	if errorCode(err) == tools.CodeArgumentsIncomplete || h.echo.n() != 0 {
		t.Fatalf("被当成工具参数截断: %+v %v exec=%d", out, err, h.echo.n())
	}
}

func TestSchemaInvalidReplacementDoesNotCountFailures(t *testing.T) {
	exportStep := llm.CallTools(llm.ToolCall{ID: "e", Name: "export_data", Arguments: `{"object":"contacts"}`})
	h := newHarness(t, echoSpec(), exportStep, exportStep, exportStep, llm.Say("结束"))
	exp := h.add(tools.SpecExportData)
	bad := approval.Response{Action: approval.ActionApprove, Params: []byte(`{"object":1}`)}
	h.approver.Responses = []approval.Response{bad, bad, bad}
	out, err := h.run()
	if err != nil || out.State != StateCompleted || out.ModelCalls != 4 || exp.n() != 0 {
		t.Fatalf("%+v %v exec=%d", out, err, exp.n())
	}
	text := requestText(h.provider.requests()[3])
	if !strings.Contains(text, tools.CodeArgumentsInvalid) {
		t.Fatalf("校验错误没有交回给模型: %s", text)
	}
	events := h.log.Events()
	if len(events) != 3 {
		t.Fatalf("替换参数失败应留下 3 条记录，实际 %+v", events)
	}
	for _, ev := range events {
		if ev.Kind != tools.CodeArgumentsInvalid || ev.Status != "failed" || ev.Code != tools.CodeArgumentsInvalid {
			t.Fatalf("记录 %+v", ev)
		}
		if strings.Contains(ev.Summary, "object") {
			t.Fatalf("摘要带了参数 %q", ev.Summary)
		}
	}
}

func TestFailedAndSkippedCallsAreLogged(t *testing.T) {
	t.Run("非法参数", func(t *testing.T) {
		h := newHarness(t, echoSpec(), badStep("b"), llm.Say("结束"))
		out, err := h.run()
		if err != nil || out.State != StateCompleted || h.echo.n() != 0 {
			t.Fatalf("%+v %v exec=%d", out, err, h.echo.n())
		}
		events := h.log.Events()
		if len(events) != 1 || events[0].Kind != tools.CodeArgumentsInvalid || events[0].Status != "failed" {
			t.Fatalf("记录 %+v", events)
		}
		if strings.Contains(events[0].Summary, `{"q":`) {
			t.Fatalf("摘要带了参数 %q", events[0].Summary)
		}
	})

	t.Run("同一步跳过和失败", func(t *testing.T) {
		h := newHarness(t, echoSpec(), llm.CallTools(
			echoCall("a", `{"q":"1"}`),
			echoCall("b", `{}`),
		), llm.Say("结束"))
		out, err := h.run()
		if err != nil || out.State != StateCompleted || h.echo.n() != 0 {
			t.Fatalf("%+v %v", out, err)
		}
		events := h.log.Events()
		if len(events) != 2 {
			t.Fatalf("记录 %+v", events)
		}
		if events[0].Kind != tools.CodeCallSkipped || events[0].Status != "skipped" {
			t.Fatalf("跳过 %+v", events[0])
		}
		if events[1].Kind != tools.CodeArgumentsInvalid || events[1].Status != "failed" {
			t.Fatalf("失败 %+v", events[1])
		}
	})

	t.Run("执行失败", func(t *testing.T) {
		h := newHarness(t, echoSpec(), goodStep("ok", "x"), llm.Say("结束"))
		h.echo.fn = func(context.Context, tools.Call) (tools.Result, error) {
			return tools.Result{}, errors.New("数据库暂时不可用")
		}
		out, err := h.run()
		if err != nil || out.State != StateCompleted || h.echo.n() != 1 {
			t.Fatalf("%+v %v exec=%d", out, err, h.echo.n())
		}
		events := h.log.Events()
		if len(events) != 1 || events[0].Kind != "tool_failed" || events[0].Status != "failed" || events[0].Code != "tool_failed" {
			t.Fatalf("记录 %+v", events)
		}
	})

	t.Run("未知工具", func(t *testing.T) {
		h := newHarness(t, echoSpec(), llm.CallTools(llm.ToolCall{ID: "u", Name: "missing", Arguments: `{}`}), llm.Say("结束"))
		out, err := h.run()
		if err != nil || out.State != StateCompleted {
			t.Fatalf("%+v %v", out, err)
		}
		events := h.log.Events()
		if len(events) != 1 || events[0].Kind != tools.CodeToolNotFound || events[0].Status != "failed" {
			t.Fatalf("记录 %+v", events)
		}
	})
}

func TestExportArgsCannotCarryConsumeContext(t *testing.T) {
	h := newHarness(t, echoSpec(), llm.CallTools(llm.ToolCall{
		ID: "e", Name: "export_data",
		Arguments: `{"object":"contacts","run_id":"seed","step_id":"seed","args_hash":"abcd"}`,
	}), llm.Say("结束"))
	exp := h.add(tools.SpecExportData)
	out, err := h.run()
	if err != nil || out.State != StateCompleted || out.ModelCalls != 2 || exp.n() != 0 || len(h.tickets.list()) != 0 {
		t.Fatalf("%+v %v exec=%d tickets=%d", out, err, exp.n(), len(h.tickets.list()))
	}
	if !strings.Contains(requestText(h.provider.requests()[1]), tools.CodeArgumentsInvalid) {
		t.Fatal("多出来的运行和步骤字段应被 Schema 拒绝")
	}
}

func TestMachinePersistsApprovalDecision(t *testing.T) {
	exportStep := llm.CallTools(llm.ToolCall{ID: "e", Name: "export_data", Arguments: `{"object":"contacts"}`})

	t.Run("批准", func(t *testing.T) {
		ws := openRuns(t)
		h := newHarness(t, echoSpec(), exportStep, llm.Say("结束"))
		exp := h.add(tools.SpecExportData)
		h.loop.Tickets = approval.New(ws.Runs)
		h.approver.Responses = []approval.Response{{Action: approval.ActionApprove}}
		out, err := h.run()
		if err != nil || out.State != StateCompleted || exp.n() != 1 {
			t.Fatalf("%+v %v exec=%d", out, err, exp.n())
		}
		var state, stepID, decision string
		var version int64
		if err := ws.Runs.QueryRowContext(t.Context(), `SELECT state, version, step_id, decision FROM approvals`).Scan(&state, &version, &stepID, &decision); err != nil {
			t.Fatal(err)
		}
		if state != string(approval.StateConsumed) || version != 2 || stepID != "e" || decision != string(approval.ActionApprove) {
			t.Fatalf("state=%s version=%d step=%s decision=%s", state, version, stepID, decision)
		}
	})

	t.Run("拒绝", func(t *testing.T) {
		ws := openRuns(t)
		h := newHarness(t, echoSpec(), exportStep, llm.Say("不该走到这里"))
		exp := h.add(tools.SpecExportData)
		h.loop.Tickets = approval.New(ws.Runs)
		h.approver.Responses = []approval.Response{{Action: approval.ActionReject}}
		out, err := h.run()
		if out.State != StateRejected || exp.n() != 0 {
			t.Fatalf("%+v %v exec=%d", out, err, exp.n())
		}
		var state string
		var version int64
		if err := ws.Runs.QueryRowContext(t.Context(), `SELECT state, version FROM approvals`).Scan(&state, &version); err != nil {
			t.Fatal(err)
		}
		if state != string(approval.StateRejected) || version != 1 {
			t.Fatalf("state=%s version=%d", state, version)
		}
	})

	t.Run("替换参数不符合 Schema 时不写决定", func(t *testing.T) {
		ws := openRuns(t)
		h := newHarness(t, echoSpec(), exportStep, llm.Say("结束"))
		exp := h.add(tools.SpecExportData)
		h.loop.Tickets = approval.New(ws.Runs)
		h.approver.Responses = []approval.Response{{
			Action: approval.ActionApprove,
			Params: []byte(`{"object":1}`),
		}}
		out, err := h.run()
		if err != nil || out.State != StateCompleted || exp.n() != 0 {
			t.Fatalf("%+v %v exec=%d", out, err, exp.n())
		}
		var state, decision string
		var version int64
		if err := ws.Runs.QueryRowContext(t.Context(), `SELECT state, version, decision FROM approvals`).Scan(&state, &version, &decision); err != nil {
			t.Fatal(err)
		}
		if state != string(approval.StatePending) || version != 0 || decision != "" {
			t.Fatalf("state=%s version=%d decision=%s", state, version, decision)
		}
	})
}

func TestConsumeBeforeExecution(t *testing.T) {
	exportCall := llm.ToolCall{ID: "e", Name: "export_data", Arguments: `{"object":"contacts"}`}

	t.Run("执行后已消费", func(t *testing.T) {
		h, exp, _, db := newExportHarness(t, exportCall)
		out, err := h.run()
		if err != nil || out.State != StateCompleted || exp.n() != 1 {
			t.Fatalf("%+v %v exec=%d", out, err, exp.n())
		}
		state, stepID, decision, version, consumed := approvalRow(t, db)
		ticket := h.approver.Tickets()[0]
		if state != string(approval.StateConsumed) || version != 2 || !consumed.Valid || consumed.String == "" {
			t.Fatalf("state=%s version=%d consumed=%v", state, version, consumed)
		}
		if stepID != ticket.StepID || stepID != "e" || decision != string(approval.ActionApprove) {
			t.Fatalf("step=%s ticket=%s decision=%s", stepID, ticket.StepID, decision)
		}
	})

	t.Run("空的调用号只生成一次步骤", func(t *testing.T) {
		ws := openRuns(t)
		h := newHarness(t, echoSpec(), llm.Say("结束"))
		exp := h.add(tools.SpecExportData)
		h.loop.Tickets = approval.New(ws.Runs)
		h.approver.Responses = []approval.Response{{Action: approval.ActionApprove}}
		prepared, err := h.reg.Prepare(tools.CallInput{Name: "export_data", Arguments: `{"object":"contacts"}`})
		if err != nil {
			t.Fatal(err)
		}
		item := &callItem{call: llm.ToolCall{Name: "export_data"}, prepared: prepared}
		in := Input{RunID: "run-1", WorkspaceID: "ws-1", Prompt: "导出"}
		out, stop, ok := h.loop.confirm(t.Context(), in, llm.Response{}, item, policy.Decision{NeedsConfirm: true}, 1)
		if stop || !ok || out.Err != nil {
			t.Fatalf("stop=%v ok=%v %+v", stop, ok, out)
		}
		if _, _, err := h.loop.execute(t.Context(), in, item.prepared, time.Second); err != nil {
			t.Fatal(err)
		}
		state, stepID, _, version, _ := approvalRow(t, ws.Runs)
		ticket := h.approver.Tickets()[0]
		if ticket.StepID == "" || stepID != ticket.StepID || state != string(approval.StateConsumed) || version != 2 || exp.n() != 1 {
			t.Fatalf("state=%s version=%d step=%s ticket=%s exec=%d", state, version, stepID, ticket.StepID, exp.n())
		}
	})

	t.Run("再次消费失败且不再执行", func(t *testing.T) {
		h, exp, m, _ := newExportHarness(t, exportCall)
		out, err := h.run()
		if err != nil || out.State != StateCompleted || exp.n() != 1 {
			t.Fatalf("%+v %v exec=%d", out, err, exp.n())
		}
		ticket := h.approver.Tickets()[0]
		// 消费成功才执行。第一次已经消费过，这里必须失败，工具次数停在 1。
		consumeErr := executeIfConsumed(t.Context(), m, ticket, exp)
		if consumeErr == nil || exp.n() != 1 {
			t.Fatalf("再次消费=%v exec=%d", consumeErr, exp.n())
		}
		if !errors.Is(consumeErr, approval.ErrIllegalTransition) || !errors.Is(consumeErr, approval.ErrTerminal) {
			t.Fatalf("再次消费 = %v", consumeErr)
		}
	})

	t.Run("条件更新冲突时不执行", func(t *testing.T) {
		h, exp, m, db := newExportHarness(t, exportCall)
		hits := 0
		m.SetGate(func() {
			hits++
			if hits != 2 {
				return
			}
			// 循环已经读到 approved，写锁还没加上。这里先把同一版本消费掉。
			m.SetGate(nil)
			var id, runID, stepID, hash string
			if err := db.QueryRowContext(context.Background(), `SELECT id, run_id, step_id, params_hash FROM approvals`).Scan(&id, &runID, &stepID, &hash); err != nil {
				t.Errorf("读取确认单: %v", err)
				return
			}
			if err := m.Consume(context.Background(), id, approval.ConsumeContext{RunID: runID, StepID: stepID, ArgsHash: hash}); err != nil {
				t.Errorf("抢先消费: %v", err)
			}
		})
		out, err := h.run()
		if hits != 2 || exp.n() != 0 || !errors.Is(err, approval.ErrVersionConflict) || out.State != StateFailed {
			t.Fatalf("hits=%d exec=%d %+v %v", hits, exp.n(), out, err)
		}
	})

	t.Run("重复同一决定只执行一次", func(t *testing.T) {
		h, exp, m, db := newExportHarness(t, exportCall)
		out, err := h.run()
		if err != nil || out.State != StateCompleted || exp.n() != 1 {
			t.Fatalf("%+v %v exec=%d", out, err, exp.n())
		}
		state, stepID, decision, version, consumed := approvalRow(t, db)
		if state != string(approval.StateConsumed) || version != 2 || !consumed.Valid {
			t.Fatalf("第一次执行后 state=%s version=%d consumed=%v", state, version, consumed)
		}
		ticket := h.approver.Tickets()[0]
		// 同一个批准再提交一次：不改状态、不改版本，工具不再跑。
		err = m.Decide(t.Context(), ticket.ID, approval.Decision{Action: approval.ActionApprove})
		if err == nil {
			_, _ = exp.Execute(t.Context(), tools.Call{Name: ticket.Action, Args: ticket.Params})
		}
		if exp.n() != 1 {
			t.Fatalf("重复决定又执行了 %d 次", exp.n())
		}
		if !errors.Is(err, approval.ErrIllegalTransition) {
			t.Fatalf("重复决定 = %v", err)
		}
		againState, againStep, againDecision, againVersion, againConsumed := approvalRow(t, db)
		if againState != state || againVersion != version || againStep != stepID || againDecision != decision || againConsumed != consumed {
			t.Fatalf("重复决定改了确认单: %s/%d/%s -> %s/%d/%s", state, version, consumed.String, againState, againVersion, againConsumed.String)
		}
	})
}

func TestStoreThatCannotDecideDoesNotExecute(t *testing.T) {
	h := newHarness(t, echoSpec(), llm.CallTools(llm.ToolCall{
		ID: "e", Name: "export_data", Arguments: `{"object":"contacts"}`,
	}), llm.Say("不该走到这里"))
	exp := h.add(tools.SpecExportData)
	h.loop.Tickets = createOnlyStore{}
	h.approver.Responses = []approval.Response{{Action: approval.ActionApprove}}
	out, err := h.run()
	if err == nil || out.State != StateFailed || exp.n() != 0 || strings.Contains(out.Text, "不该走到这里") {
		t.Fatalf("%+v %v exec=%d", out, err, exp.n())
	}
	if !strings.Contains(err.Error(), "不能写决定") {
		t.Fatalf("错误 = %v", err)
	}
}

func TestDeciderWithoutConsumeDoesNotExecute(t *testing.T) {
	h := newHarness(t, echoSpec(), llm.CallTools(llm.ToolCall{
		ID: "e", Name: "export_data", Arguments: `{"object":"contacts"}`,
	}), llm.Say("不该走到这里"))
	exp := h.add(tools.SpecExportData)
	store := &decideOnlyStore{}
	h.loop.Tickets = store
	h.approver.Responses = []approval.Response{{Action: approval.ActionApprove}}
	out, err := h.run()
	if store.decisions != 1 || err == nil || out.State != StateFailed || exp.n() != 0 {
		t.Fatalf("decisions=%d %+v %v exec=%d", store.decisions, out, err, exp.n())
	}
	if !strings.Contains(err.Error(), "不能消费") {
		t.Fatalf("错误 = %v", err)
	}
}

func newExportHarness(t *testing.T, call llm.ToolCall) (*harness, *fnTool, *approval.Machine, *sql.DB) {
	t.Helper()
	ws := openRuns(t)
	h := newHarness(t, echoSpec(), llm.CallTools(call), llm.Say("结束"))
	exp := h.add(tools.SpecExportData)
	m := approval.New(ws.Runs)
	h.loop.Tickets = m
	h.approver.Responses = []approval.Response{{Action: approval.ActionApprove}}
	return h, exp, m, ws.Runs
}

// executeIfConsumed 和循环一样：消费的条件更新成功才执行工具。
func executeIfConsumed(ctx context.Context, m *approval.Machine, ticket approval.Ticket, tool *fnTool) error {
	err := m.Consume(ctx, ticket.ID, approval.ConsumeContext{
		RunID:    ticket.RunID,
		StepID:   ticket.StepID,
		ArgsHash: ticket.ParamsHash,
	})
	if err != nil {
		return err
	}
	_, err = tool.Execute(ctx, tools.Call{Name: ticket.Action, Args: ticket.Params})
	return err
}

func approvalRow(t *testing.T, db *sql.DB) (state, stepID, decision string, version int64, consumed sql.NullString) {
	t.Helper()
	err := db.QueryRowContext(t.Context(), `SELECT state, version, step_id, decision, consumed_at FROM approvals`).Scan(
		&state, &version, &stepID, &decision, &consumed,
	)
	if err != nil {
		t.Fatal(err)
	}
	return
}

type createOnlyStore struct{}

func (createOnlyStore) Create(ctx context.Context, ticket approval.Ticket) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ticket.Validate()
}

type decideOnlyStore struct {
	decisions int
}

func (s *decideOnlyStore) Create(ctx context.Context, ticket approval.Ticket) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ticket.Validate()
}

func (s *decideOnlyStore) Decide(ctx context.Context, id string, decision approval.Decision) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if id == "" {
		return approval.ErrNotFound
	}
	if err := decision.Validate(); err != nil {
		return err
	}
	s.decisions++
	return nil
}
