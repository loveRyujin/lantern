package session

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/approval"
	"github.com/loveRyujin/lantern/core/run"
)

func TestPlainTextEscapesControlAndBidi(t *testing.T) {
	in := "a\x1b[31mred\x1b[0m\n2026-01-01  确认决定 · 批准\u202egnp.exe\x00\t"
	got := PlainText(in)
	for _, bad := range []string{"\x1b", "\n", "\u202e", "\x00", "\t"} {
		if strings.Contains(got, bad) {
			t.Fatalf("没有转义 %q: %q", bad, got)
		}
	}
	for _, want := range []string{`\x1b[31m`, `\n2026-01-01`, `\u202e`, `\x00`, `\t`} {
		if !strings.Contains(got, want) {
			t.Fatalf("缺少 %q: %q", want, got)
		}
	}
	if PlainText("**粗体** `code`") != "**粗体** `code`" {
		t.Fatal("普通文本被改写")
	}
}

// 参数里的换行不能在命令行里伪造出一行新的轨迹。
func TestFormatTraceKeepsOneEntryPerLine(t *testing.T) {
	at := time.Date(2026, 11, 20, 1, 2, 3, 0, time.UTC)
	tr := run.Trace{RunID: "r", State: run.StateCompleted, CreatedAt: at, UpdatedAt: at, Entries: []run.TraceEntry{{
		At: at, Source: run.TraceTool, Kind: "run_sql", Tool: "run_sql", Status: run.StepDone,
		Input: "{\"sql\":\"SELECT 1\"}\n2026-11-20 09:00:00.000  确认决定 · 批准 · export_data",
	}}}
	out := FormatTrace("ws", tr)
	heads := 0
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.Contains(line, "确认决定") && !strings.HasPrefix(line, "    参数 ") {
			t.Fatalf("参数伪造了一行轨迹:\n%s", out)
		}
		if strings.Contains(line, "  工具 run_sql") {
			heads++
		}
	}
	if heads != 1 {
		t.Fatalf("工具行 %d:\n%s", heads, out)
	}
}

func TestTraceErrorCarriesNoRecordContent(t *testing.T) {
	cases := map[error]string{
		run.ErrNotFound:       "工作区 ws 里没有运行 r1。",
		run.ErrTraceWorkspace: "这次运行的记录不属于当前工作区，已拒绝显示。",
		errors.Join(run.ErrTraceCorrupt, errors.New("x")): "轨迹数据损坏，无法显示",
	}
	for err, want := range cases {
		if got := TraceError("ws", "r1", err); !strings.HasPrefix(got, want) {
			t.Fatalf("%v → %q", err, got)
		}
	}
}

// 重启后从待确认列表重开并批准：Resume 执行的那一步也要有结果摘要和迁移。
func TestTraceAfterResumeShowsResultAndTransitions(t *testing.T) {
	ctx := t.Context()
	r, tool, w, root := newFlow(t, "星禾")
	done := startAsk(ctx, r, "星禾", "导出客户", "run-resume")
	tk := waitTicket(t, w, "run-resume")
	if err := w.Deliver(tk.ID, approval.Decision{Action: approval.ActionDefer}); err != nil {
		t.Fatal(err)
	}
	if res := waitAsk(t, done); res.result.RunState != StatusAwaiting {
		t.Fatalf("%+v", res)
	}
	h := openWS(t, root, "星禾")
	svc := run.New(h.Runs)
	svc.SetApprover(w)
	svc.SetExecutor(tool.Execute)
	errc := make(chan error, 1)
	go func() {
		_, err := w.Resume(ctx, svc, "run-resume")
		errc <- err
	}()
	if err := w.Deliver(waitTicket(t, w, "run-resume").ID, approval.Decision{Action: approval.ActionApprove}); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	h.Close()
	if tool.N() != 1 {
		t.Fatalf("执行 %d 次", tool.N())
	}
	tr, err := ReadTrace(ctx, root, "星禾", "run-resume")
	if err != nil {
		t.Fatal(err)
	}
	var moves []string
	var step run.TraceEntry
	for _, e := range tr.Entries {
		switch e.Source {
		case run.TraceTransition:
			moves = append(moves, e.To)
		case run.TraceTool:
			step = e
		}
	}
	if got := strings.Join(moves, ","); got != "created,running,awaiting_approval,running,completed" {
		t.Fatalf("迁移 %s", got)
	}
	if step.Status != run.StepDone || step.Summary != "已执行" || step.TicketID != tk.ID || step.ParamsHash != tk.ParamsHash || step.Duration <= 0 {
		t.Fatalf("Resume 步骤 %+v", step)
	}
}

func TestTraceExecutingStepLabelFollowsRunState(t *testing.T) {
	step := run.TraceEntry{Source: run.TraceTool, Tool: "export_data", Status: run.StepExecuting, StepID: "s1"}
	body := func(state string) string {
		lines := TraceLines(run.Trace{State: state, Entries: []run.TraceEntry{step}})
		return strings.Join(lines[0].Body, "\n")
	}
	for _, st := range []string{run.StateRunning, run.StateAwaitingApproval} {
		if got := body(st); strings.Contains(got, "执行中断") || !strings.Contains(got, "可能仍在执行") {
			t.Errorf("%s: %s", st, got)
		}
	}
	for _, st := range []string{run.StateFailed, run.StateCompleted, run.StateCancelled} {
		if got := body(st); !strings.Contains(got, "执行中断") {
			t.Errorf("%s: %s", st, got)
		}
	}
}
