package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/approval"
	"github.com/loveRyujin/lantern/core/run"
	"github.com/loveRyujin/lantern/core/tools"
	"github.com/loveRyujin/lantern/core/workspace"
)

type gatedTool struct {
	entered chan struct{}
	release chan struct{}
	n       atomic.Int64
}

func (g *gatedTool) Spec() tools.Spec { return tools.SpecExportData }

func (g *gatedTool) Execute(ctx context.Context, _ tools.Call) (tools.Result, error) {
	g.n.Add(1)
	g.entered <- struct{}{}
	select {
	case <-g.release:
	case <-ctx.Done():
		return tools.Result{}, ctx.Err()
	}
	return tools.Result{Content: "已执行", Summary: "已执行"}, nil
}

// TestAskDuringResumeKeepsExecutingRun 按 shell.reopen 的调用方式跑 Resume（OpenPrepared + Waiter.Resume），
// 工具卡在执行中时，同一工作区再走一次 LoopRunner.Ask。演示模型和 Agent 循环都不替换。
// 另一个工作区的提问作为对照，它的 Recover 只扫自己的 runs.db。
func TestAskDuringResumeKeepsExecutingRun(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	for _, id := range []string{"星禾", "云杉"} {
		h, err := workspace.Create(ctx, root, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.Close(); err != nil {
			t.Fatal(err)
		}
	}
	gate := &gatedTool{entered: make(chan struct{}, 4), release: make(chan struct{})}
	w := NewWaiter()
	r := &LoopRunner{Root: root, Waiter: w, Tool: gate, Demo: true}

	askAndDefer(t, ctx, r, w, "星禾", "run-a")

	h, err := workspace.OpenPrepared(ctx, root, "星禾")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	svc := run.New(h.Runs)
	svc.SetApprover(w)
	svc.SetExecutor(gate.Execute)
	type resumed struct {
		res run.ResumeResult
		err error
	}
	resc := make(chan resumed, 1)
	go func() {
		res, err := w.Resume(ctx, svc, "run-a")
		resc <- resumed{res, err}
	}()
	tk := waitTicket(t, w, "run-a")
	if err := w.Deliver(tk.ID, approval.Decision{Action: approval.ActionApprove}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("批准后工具没有开始执行")
	}
	st, step := roState(t, root, "星禾", "run-a")
	t.Logf("执行窗口：run-a state=%s 最后一步=%s Busy=%v", st, step, w.Busy("run-a"))
	if st != run.StateRunning || step != run.StepExecuting {
		t.Fatalf("没卡在执行窗口 state=%s step=%s", st, step)
	}

	askAndDefer(t, ctx, r, w, "云杉", "run-ctl")
	ctlState, _ := roState(t, root, "星禾", "run-a")
	t.Logf("对照：云杉提问后 run-a state=%s", ctlState)
	if ctlState != run.StateRunning {
		t.Errorf("另一个工作区的 Recover 改了星禾：%s", ctlState)
	}

	askAndDefer(t, ctx, r, w, "星禾", "run-b")
	during, _ := roState(t, root, "星禾", "run-a")
	t.Logf("同工作区第二问后、工具未返回：run-a state=%s", during)

	close(gate.release)
	var got resumed
	select {
	case got = <-resc:
	case <-time.After(5 * time.Second):
		t.Fatal("Resume 没有返回")
	}
	final, finalStep := roState(t, root, "星禾", "run-a")
	trs := roTransitions(t, root, "星禾", "run-a")
	aud := roAudits(t, root, "星禾", "run-a")
	t.Logf("Resume 返回：res=%+v err=%v 执行次数=%d", got.res, got.err, gate.n.Load())
	t.Logf("最终 run-a state=%s 最后一步=%s", final, finalStep)
	t.Logf("run_transitions(run-a)：%s", strings.Join(trs, " | "))
	t.Logf("audit(run-a)：%s", strings.Join(aud, " | "))
	t.Logf("approvals(run-a)：%s", roApproval(t, root, "星禾", "run-a"))

	if gate.n.Load() != 1 {
		t.Errorf("工具执行了 %d 次", gate.n.Load())
	}
	if during == run.StateFailed {
		t.Errorf("工具仍在执行，同工作区提问的 Recover 已把 run-a 标成 failed")
	}
	for _, a := range aud {
		if strings.HasPrefix(a, run.AuditExecutingInterrupted) {
			t.Errorf("正在执行的运行被记了 %s", a)
		}
	}
	for _, tr := range trs {
		if strings.HasSuffix(tr, "->failed") {
			t.Errorf("run_transitions 出现 %s，最终又是 %s", tr, final)
		}
	}
}

func askAndDefer(t *testing.T, ctx context.Context, r *LoopRunner, w *Waiter, ws, runID string) {
	t.Helper()
	done := startAsk(ctx, r, ws, "导出", runID)
	tk := waitTicket(t, w, runID)
	if err := w.Deliver(tk.ID, approval.Decision{Action: approval.ActionDefer}); err != nil {
		t.Fatal(err)
	}
	if res := waitAsk(t, done); res.err != nil || res.result.RunState != StatusAwaiting {
		t.Fatalf("%s/%s result=%+v err=%v", ws, runID, res.result, res.err)
	}
}

func roState(t *testing.T, root, ws, runID string) (state, lastStep string) {
	t.Helper()
	db, err := openRunsRO(t.Context(), root, ws)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRowContext(t.Context(), `SELECT state FROM runs WHERE id = ?`, runID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	_ = db.QueryRowContext(t.Context(), `SELECT status FROM run_steps WHERE run_id = ? ORDER BY ordinal DESC LIMIT 1`, runID).Scan(&lastStep)
	return state, lastStep
}

func roTransitions(t *testing.T, root, ws, runID string) []string {
	t.Helper()
	return roStrings(t, root, ws, `SELECT from_state || '->' || to_state FROM run_transitions WHERE run_id = ? ORDER BY id`, runID)
}

func roAudits(t *testing.T, root, ws, runID string) []string {
	t.Helper()
	return roStrings(t, root, ws, `SELECT action || ' ' || detail FROM audit WHERE run_id = ? ORDER BY id`, runID)
}

func roApproval(t *testing.T, root, ws, runID string) string {
	t.Helper()
	return strings.Join(roStrings(t, root, ws, `SELECT id || ' state=' || state || ' consumed=' || COALESCE(consumed_at, '') FROM approvals WHERE run_id = ?`, runID), " | ")
}

func roStrings(t *testing.T, root, ws, query, runID string) []string {
	t.Helper()
	db, err := openRunsRO(t.Context(), root, ws)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.QueryContext(t.Context(), query, runID)
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

// TestResumeRecoverStress 不加阻塞点：Resume 用普通计数工具，另一个 goroutine 反复走 workspace.Open（即 Ask 开头的那一步）。
// 统计 Resume 窗口内被 Recover 写成 failed 的次数。默认跳过，LANTERN_RACE_STRESS=迭代次数 时运行。
func TestResumeRecoverStress(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("LANTERN_RACE_STRESS"))
	if n <= 0 {
		t.Skip("设置 LANTERN_RACE_STRESS 运行")
	}
	ctx := t.Context()
	patterns := map[string]int{}
	hit := 0
	normal, retryable, anomalies := 0, 0, 0
	for i := 0; i < n; i++ {
		r, tool, w, root := newFlow(t, "星禾")
		runID := fmt.Sprintf("run-s%d", i)
		askAndDefer(t, ctx, r, w, "星禾", runID)
		h, err := workspace.OpenPrepared(ctx, root, "星禾")
		if err != nil {
			t.Fatal(err)
		}
		svc := run.New(h.Runs)
		svc.SetApprover(w)
		svc.SetExecutor(tool.Execute)
		stop := make(chan struct{})
		spun := make(chan int, 1)
		go func() {
			k := 0
			for {
				select {
				case <-stop:
					spun <- k
					return
				default:
				}
				if o, err := workspace.Open(ctx, root, "星禾"); err == nil {
					_ = o.Close()
					k++
				}
			}
		}()
		resc := make(chan error, 1)
		go func() {
			_, err := w.Resume(ctx, svc, runID)
			resc <- err
		}()
		tk := waitTicket(t, w, runID)
		if err := w.Deliver(tk.ID, approval.Decision{Action: approval.ActionApprove}); err != nil {
			t.Fatal(err)
		}
		rerr := <-resc
		close(stop)
		<-spun
		_ = h.Close()
		trs := roTransitions(t, root, "星禾", runID)
		key := strings.Join(trs[3:], " | ")
		var acts []string
		for _, a := range roAudits(t, root, "星禾", runID) {
			if act, _, _ := strings.Cut(a, " "); act != run.AuditApprovalRestored {
				acts = append(acts, act)
			}
		}
		key += fmt.Sprintf(" ; audit=%v ; 执行=%d ; 票据=%s", acts, tool.N(), roStrings(t, root, "星禾", `SELECT state FROM approvals WHERE run_id = ?`, runID))
		if rerr != nil {
			key += " ; resume err: " + rerr.Error()
		}
		patterns[key]++
		for _, tr := range trs {
			if strings.HasSuffix(tr, "->failed") {
				hit++
				break
			}
		}
		tickets := roStrings(t, root, "星禾", `SELECT state FROM approvals WHERE run_id = ?`, runID)
		state, _ := roState(t, root, "星禾", runID)
		switch stressOutcome(trs[3:], acts, tool.N(), tickets, state, rerr) {
		case "normal":
			normal++
		case "retryable":
			retryable++
		default:
			anomalies++
		}
	}
	t.Logf("迭代 %d 次，Resume 期间被写成 failed 的 %d 次", n, hit)
	t.Logf("AC3 口径：正常 %d，可重试失败 %d，异常 %d（%.3f%%）", normal, retryable, anomalies, 100*float64(anomalies)/float64(n))
	for k, v := range patterns {
		t.Logf("%4d  %s", v, k)
	}
	if anomalies > 0 {
		t.Errorf("异常 %d 次", anomalies)
	}
}

// stressOutcome 按 Issue AC3 归类批准之后的结果。
// normal：awaiting→running→completed，执行 1 次，票据 consumed，没有恢复类审计，Resume 无错。
// retryable：Resume 返回 run.ErrBusy，票据没有核销，运行仍是 awaiting_approval，执行 0 次。
// 其余都是异常。
func stressOutcome(after, acts []string, executed int64, tickets []string, state string, rerr error) string {
	recovered := false
	for _, a := range acts {
		switch a {
		case run.AuditExecutingInterrupted, run.AuditIllegalResidual, run.AuditBreakpointHeld:
			recovered = true
		}
	}
	oneTicket := func(want ...string) bool {
		if len(tickets) != 1 {
			return false
		}
		for _, w := range want {
			if tickets[0] == w {
				return true
			}
		}
		return false
	}
	if rerr == nil && !recovered && executed == 1 && oneTicket("consumed") && state == run.StateCompleted &&
		strings.Join(after, " | ") == "awaiting_approval->running | running->completed" {
		return "normal"
	}
	if errors.Is(rerr, run.ErrBusy) && !recovered && executed == 0 && oneTicket("pending", "approved") &&
		state == run.StateAwaitingApproval && len(after) == 0 {
		return "retryable"
	}
	return "anomaly"
}
