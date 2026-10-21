package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/loveRyujin/lantern/core/approval"
	"github.com/loveRyujin/lantern/core/llm"
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
		if state != string(approval.StateApproved) || version != 1 || stepID != "e" || decision != string(approval.ActionApprove) {
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
