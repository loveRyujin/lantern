package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/loveRyujin/lantern/core/approval"
	"github.com/loveRyujin/lantern/core/llm"
	"github.com/loveRyujin/lantern/core/tools"
)

type stepSpy struct {
	begins int
}

func (s *stepSpy) Begin(context.Context, StepMark) error {
	s.begins++
	return nil
}

func (s *stepSpy) Finish(context.Context, string, string) error { return nil }

func TestRunPythonOverPrivilegeDoesNotExecuteWithoutConfirm(t *testing.T) {
	args := `{"code":"print(1)","limits":{"wall_ms":45000,"cpu_ms":20000},"extra_read_only":["/data/ro"],"approved":true,"requires_approval":false}`
	h := newHarness(t, tools.SpecRunPython, llm.CallTools(llm.ToolCall{
		ID: "p", Name: "run_python", Arguments: args,
	}))
	spy := &stepSpy{}
	h.loop.Steps = spy
	h.approver.Responses = []approval.Response{{Action: approval.ActionDefer}}
	var executed string
	h.echo.fn = func(_ context.Context, call tools.Call) (tools.Result, error) {
		executed = string(call.Args)
		return tools.Result{Content: "ran", Summary: "ran"}, nil
	}
	out, err := h.run()
	if err != nil || out.State != StateAwaitingApproval || h.echo.n() != 0 || executed != "" || spy.begins != 0 {
		t.Fatalf("未确认仍执行: state=%s err=%v exec=%d begins=%d %+v", out.State, err, h.echo.n(), spy.begins, out)
	}
	tickets := h.tickets.list()
	if len(tickets) != 1 || len(h.approver.Tickets()) != 1 {
		t.Fatalf("确认单数量 tickets=%d approver=%d", len(tickets), len(h.approver.Tickets()))
	}
	if string(tickets[0].Params) != args || tickets[0].Action != "run_python" {
		t.Fatalf("冻结参数不对: %+v", tickets[0])
	}
	for _, piece := range []string{"45000ms", "30000ms", "20000ms", "10000ms", "/data/ro"} {
		if !strings.Contains(tickets[0].Risk, piece) || !strings.Contains(tickets[0].Risk, "默认") {
			t.Fatalf("确认单没有写清差额 %s: %s", piece, tickets[0].Risk)
		}
	}
}

func TestRunPythonNetworkDenyCreatesNoTicket(t *testing.T) {
	for _, args := range []string{
		`{"code":"print(1)","network":true}`,
		`{"code":"print(1)","network":"loopback"}`,
		`{"code":"print(1)","network":"127.0.0.1"}`,
		`{"code":"print(1)","network":"::1","approved":true,"requires_approval":false}`,
		`{"code":"print(1)","network":"localhost"}`,
		`{"code":"print(1)","limits":{"nproc":2}}`,
		`{"code":"print(1)","limits":{"cpu_ms":30001}}`,
		`{"code":"print(1)","limits":{"memory_bytes":1073741825},"approved":true}`,
	} {
		t.Run(args, func(t *testing.T) {
			h := newHarness(t, tools.SpecRunPython,
				llm.CallTools(llm.ToolCall{ID: "p", Name: "run_python", Arguments: args}),
				llm.Say("结束"),
			)
			spy := &stepSpy{}
			h.loop.Steps = spy
			h.loop.Policy = nil
			h.approver.Responses = []approval.Response{{Action: approval.ActionApprove}}
			out, err := h.run()
			if err != nil || out.State != StateCompleted || h.echo.n() != 0 || spy.begins != 0 {
				t.Fatalf("拒绝后仍执行: state=%s err=%v exec=%d begins=%d", out.State, err, h.echo.n(), spy.begins)
			}
			if len(h.tickets.list()) != 0 || len(h.approver.Tickets()) != 0 || h.pol.n != 0 {
				t.Fatalf("拒绝创建了确认单 tickets=%d approver=%d policy=%d", len(h.tickets.list()), len(h.approver.Tickets()), h.pol.n)
			}
			if len(h.provider.requests()) < 2 {
				t.Fatal("拒绝没有回到模型")
			}
			feedback := requestText(h.provider.requests()[1])
			if !strings.Contains(feedback, "策略拒绝执行 run_python") || !strings.Contains(feedback, "不能通过确认放开") {
				t.Fatalf("模型没有看到拒绝原因: %s", feedback)
			}
		})
	}
}

func TestRunPythonDefaultAllowanceExecutes(t *testing.T) {
	for _, args := range []string{
		`{"code":"print(1)"}`,
		`{"code":"print(1)","sql_call_id":"query-1"}`,
		`{"code":"print(1)","limits":{"cpu_ms":10000,"memory_bytes":536870912,"wall_ms":30000,"nproc":1},"extra_read_only":[],"network":false}`,
		`{"code":"print(1)","limits":{"cpu_ms":1000,"memory_bytes":1,"wall_ms":1,"nproc":0}}`,
		`{"code":"print(1)","approved":true,"requires_approval":false}`,
	} {
		t.Run(args, func(t *testing.T) {
			h := newHarness(t, tools.SpecRunPython,
				llm.CallTools(llm.ToolCall{ID: "p", Name: "run_python", Arguments: args}),
				llm.Say("完成"),
			)
			spy := &stepSpy{}
			h.loop.Steps = spy
			h.approver.Responses = []approval.Response{{Action: approval.ActionApprove}}
			var got string
			h.echo.fn = func(_ context.Context, call tools.Call) (tools.Result, error) {
				got = string(call.Args)
				return tools.Result{Content: "ok", Summary: "ok"}, nil
			}
			out, err := h.run()
			if err != nil || out.State != StateCompleted || h.echo.n() != 1 || got != args || spy.begins != 1 {
				t.Fatalf("默认权限没有执行: state=%s err=%v exec=%d begins=%d args=%s", out.State, err, h.echo.n(), spy.begins, got)
			}
			if len(h.tickets.list()) != 0 || len(h.approver.Tickets()) != 0 {
				t.Fatalf("默认权限进入了确认 tickets=%d approver=%d", len(h.tickets.list()), len(h.approver.Tickets()))
			}
		})
	}
}

func TestRunPythonApprovedOverPrivilegeReachesToolWithFrozenArgs(t *testing.T) {
	args := `{"code":"print(1)","limits":{"wall_ms":45000},"extra_read_only":["/data/ro"]}`
	h := newHarness(t, tools.SpecRunPython,
		llm.CallTools(llm.ToolCall{ID: "p", Name: "run_python", Arguments: args}),
		llm.Say("完成"),
	)
	spy := &stepSpy{}
	h.loop.Steps = spy
	h.approver.Responses = []approval.Response{{Action: approval.ActionApprove}}
	var got string
	var approvalID string
	h.echo.fn = func(_ context.Context, call tools.Call) (tools.Result, error) {
		got = string(call.Args)
		approvalID = call.ApprovalID
		return tools.Result{Content: "ok", Summary: "ok"}, nil
	}
	out, err := h.run()
	if err != nil || out.State != StateCompleted || h.echo.n() != 1 || got != args || approvalID == "" || spy.begins != 1 {
		t.Fatalf("确认后没有按冻结参数执行: state=%s err=%v exec=%d id=%q args=%s", out.State, err, h.echo.n(), approvalID, got)
	}
	if len(h.tickets.list()) != 1 || len(h.approver.Tickets()) != 1 {
		t.Fatalf("确认路径没有走确认单 tickets=%d approver=%d", len(h.tickets.list()), len(h.approver.Tickets()))
	}
}
