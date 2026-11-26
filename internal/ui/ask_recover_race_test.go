package ui

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/approval"
	"github.com/loveRyujin/lantern/core/llm"
	"github.com/loveRyujin/lantern/core/run"
	"github.com/loveRyujin/lantern/core/tools"
	"github.com/loveRyujin/lantern/core/workspace"
	"github.com/loveRyujin/lantern/internal/ui/session"
)

// gatedExport 注册成 export_data。Execute 进入后先通知测试，再等测试放行，
// 用来把 Resume 卡在“步骤 executing、运行 running”这个窗口里。
type gatedExport struct {
	entered chan struct{}
	release chan struct{}
	n       atomic.Int64
}

func newGatedExport() *gatedExport {
	return &gatedExport{entered: make(chan struct{}, 4), release: make(chan struct{})}
}

func (g *gatedExport) Spec() tools.Spec { return tools.SpecExportData }

func (g *gatedExport) Execute(ctx context.Context, _ tools.Call) (tools.Result, error) {
	g.n.Add(1)
	g.entered <- struct{}{}
	select {
	case <-g.release:
	case <-ctx.Done():
		return tools.Result{}, ctx.Err()
	}
	return tools.Result{Content: "已执行", Summary: "已执行"}, nil
}

// 第一问让模型调 export_data，之后的提问只回答文字，不再开确认单。
func raceProvider() func(string) llm.Provider {
	var calls atomic.Int64
	return func(runID string) llm.Provider {
		if calls.Add(1) > 1 {
			return llm.NewMock(llm.MustNewScript(llm.Say("第二问的回答。")))
		}
		raw, _ := json.Marshal(map[string]any{"object": "客户名单", "fields": []string{"phone"}})
		return llm.NewMock(llm.MustNewScript(
			llm.CallTools(llm.ToolCall{ID: "call-" + runID, Name: "export_data", Arguments: string(raw)}),
			llm.Say("第一问结束。"),
		))
	}
}

// TestShellAskDuringResumeKeepsExecutingRun 走桌面的真实路径：
// 提问 → 暂缓 → 待确认列表“重开” → 对话框“批准” → Resume 执行工具（卡住）→ 再点“发送”提新问题。
// 第二次提问的 LoopRunner.Ask 会经 workspace.Open 跑 Recover。正在执行的运行不应被标成 failed。
func TestShellAskDuringResumeKeepsExecutingRun(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	const ws = "星禾"
	h, err := workspace.Create(ctx, root, ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	gate := newGatedExport()
	waiter := session.NewWaiter()
	runner := &session.LoopRunner{Root: root, Waiter: waiter, Tool: gate, NewProvider: raceProvider(), Demo: true}
	s, err := newShell(ctx, Options{Root: root, Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	if !s.state.Switch(ws) {
		t.Fatal("没有星禾")
	}
	tt := tester(s)
	tt.Frame()

	// 第一问在无窗口模式下同步跑完。确认框照常交给 shell.showTicket，旁路只多收一份票据并暂缓，
	// 让运行停在 awaiting_approval。
	shown := make(chan approval.Ticket, 4)
	waiter.SetOnShow(func(tk approval.Ticket) {
		s.showTicket(tk)
		shown <- tk
	})
	deferred := make(chan error, 1)
	go func() {
		select {
		case tk := <-shown:
			deferred <- waiter.Deliver(tk.ID, approval.Decision{Action: approval.ActionDefer})
		case <-time.After(5 * time.Second):
			deferred <- errors.New("第一问没有等到确认单")
		}
	}()
	ask(t, tt, "导出客户手机号")
	if err := <-deferred; err != nil {
		t.Fatal(err)
	}
	// 无窗口模式下第一问同步返回，模态框还开着。点一次“暂缓”把界面收成用户手点暂缓后的样子；
	// 决定已经送出，这次 Deliver 返回 ErrNoWait 被 confirm 忽略。
	tt.Frame()
	if err := tt.Click("暂缓"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if s.dialogOpen() {
		t.Fatal("暂缓后对话框仍开着")
	}
	tt.Frame()
	s.reloadList()
	list := shellList(s)
	if len(list) != 1 {
		t.Fatalf("待确认列表=%+v", list)
	}
	tk := list[0]
	runA := tk.RunID
	if st := traceOf(t, root, ws, runA).State; st != run.StateAwaitingApproval {
		t.Fatalf("暂缓后状态=%s", st)
	}

	// 点“重开”，再在对话框里点“批准”。
	tt.Frame()
	if err := tt.Click("重开 " + tk.ID); err != nil {
		t.Fatal(err)
	}
	raceWait(t, tt, "重开后的确认对话框", func() bool { return tt.HasText("批准") && waiter.Busy(runA) && hasCurrent(waiter, runA) })
	if err := tt.Click("批准"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("批准后工具没有开始执行")
	}
	before := traceOf(t, root, ws, runA)
	t.Logf("Resume 执行中、第二问之前：runs.state=%s 最后一步=%s", before.State, lastToolStatus(before))
	if before.State != run.StateRunning || lastToolStatus(before) != run.StepExecuting {
		t.Fatalf("没卡在执行窗口 state=%s step=%s", before.State, lastToolStatus(before))
	}

	// Resume 期间发送按钮仍然可点。第二问会打开工作区并跑 Recover。
	tt.Frame()
	if s.state.StreamingRun != "" {
		t.Fatalf("StreamingRun=%s", s.state.StreamingRun)
	}
	ask(t, tt, "顺便看看上个月营收")
	tt.Frame()
	runs := listRuns(t, root, ws)
	if len(runs) != 2 {
		t.Fatalf("第二问没有落库 runs=%+v", runs)
	}
	during := traceOf(t, root, ws, runA)
	t.Logf("第二问返回后、工具仍未返回：runs.state=%s 最后一步=%s", during.State, lastToolStatus(during))
	t.Logf("此刻轨迹视图（UI 读同一份数据）：\n%s", session.FormatTrace(ws, during))
	duringPhase := s.state.Phase()

	close(gate.release)
	raceWait(t, tt, "Resume 收尾", func() bool {
		return shellNotice(s) == "已按冻结参数执行一次。没有继续问模型。" || strings.Contains(shellNotice(s), "run:")
	})
	final := traceOf(t, root, ws, runA)
	t.Logf("Resume 返回后：runs.state=%s notice=%q 执行次数=%d 第二问时 Phase=%q", final.State, shellNotice(s), gate.n.Load(), duringPhase)
	t.Logf("run_transitions(%s)：%s", runA, strings.Join(transitions(final), " | "))
	t.Logf("audit(%s)：%s", runA, strings.Join(audits(final), " | "))
	t.Logf("approvals(%s)：%s", runA, strings.Join(approvalStages(final), " | "))

	if gate.n.Load() != 1 {
		t.Errorf("工具执行了 %d 次", gate.n.Load())
	}
	if during.State == run.StateFailed {
		t.Errorf("工具仍在执行，第二问的 Recover 已把 %s 标成 failed", runA)
	}
	for _, a := range audits(final) {
		if strings.HasPrefix(a, run.AuditExecutingInterrupted) {
			t.Errorf("正在执行的运行被记了 %s", a)
		}
	}
	for _, tr := range transitions(final) {
		if strings.HasSuffix(tr, "->failed") {
			t.Errorf("run_transitions 出现 %s，随后又被改成 %s", tr, final.State)
		}
	}
}

func hasCurrent(w *session.Waiter, runID string) bool {
	_, ok := w.Current(runID)
	return ok
}

func raceWait(t *testing.T, tt interface{ Frame() }, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		tt.Frame()
		if ok() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("没有等到%s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func traceOf(t *testing.T, root, ws, runID string) run.Trace {
	t.Helper()
	tr, err := session.ReadTrace(t.Context(), root, ws, runID)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func listRuns(t *testing.T, root, ws string) []run.RunSummary {
	t.Helper()
	out, err := session.ListRuns(t.Context(), root, ws, 10)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func lastToolStatus(tr run.Trace) string {
	status := ""
	for _, e := range tr.Entries {
		if e.Source == run.TraceTool {
			status = e.Status
		}
	}
	return status
}

func transitions(tr run.Trace) []string {
	var out []string
	for _, e := range tr.Entries {
		if e.Source == run.TraceTransition {
			out = append(out, e.From+"->"+e.To)
		}
	}
	return out
}

func audits(tr run.Trace) []string {
	var out []string
	for _, e := range tr.Entries {
		if e.Source == run.TraceAudit {
			out = append(out, e.Kind+" "+e.Detail)
		}
	}
	return out
}

func approvalStages(tr run.Trace) []string {
	var out []string
	for _, e := range tr.Entries {
		if e.Source == run.TraceApproval {
			out = append(out, e.Kind+":"+e.Decision)
		}
	}
	return out
}
