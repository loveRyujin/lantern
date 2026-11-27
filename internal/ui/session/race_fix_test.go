package session

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/approval"
	"github.com/loveRyujin/lantern/core/run"
	"github.com/loveRyujin/lantern/core/tools"
	"github.com/loveRyujin/lantern/core/workspace"
)

// failingGate 和 gatedTool 一样停在 Execute 里，放行后返回错误。
type failingGate struct {
	entered chan struct{}
	release chan struct{}
	n       atomic.Int64
}

func (g *failingGate) Spec() tools.Spec { return tools.SpecExportData }

func (g *failingGate) Execute(ctx context.Context, _ tools.Call) (tools.Result, error) {
	g.n.Add(1)
	g.entered <- struct{}{}
	select {
	case <-g.release:
	case <-ctx.Done():
		return tools.Result{}, ctx.Err()
	}
	return tools.Result{}, errors.New("导出失败")
}

// 失败路径：Resume 执行中同工作区再提问，工具随后返回错误。运行只因工具失败而 failed，
// 没有 executing_interrupted，迁移里没有"先失败后又改"，票据不会再出现在待确认列表。
func TestAskDuringResumeToolFailureStaysToolFailed(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	h0, err := workspace.Create(ctx, root, "星禾")
	if err != nil {
		t.Fatal(err)
	}
	_ = h0.Close()
	gate := &failingGate{entered: make(chan struct{}, 4), release: make(chan struct{})}
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
	resc := make(chan error, 1)
	go func() {
		_, err := w.Resume(ctx, svc, "run-a")
		resc <- err
	}()
	tk := waitTicket(t, w, "run-a")
	if err := w.Deliver(tk.ID, approval.Decision{Action: approval.ActionApprove}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("工具没有开始执行")
	}
	askAndDefer(t, ctx, r, w, "星禾", "run-b")
	if st, step := roState(t, root, "星禾", "run-a"); st != run.StateRunning || step != run.StepExecuting {
		t.Fatalf("第二问之后 run-a state=%s step=%s", st, step)
	}
	close(gate.release)
	var rerr error
	select {
	case rerr = <-resc:
	case <-time.After(5 * time.Second):
		t.Fatal("Resume 没有返回")
	}
	if rerr == nil || gate.n.Load() != 1 {
		t.Fatalf("err=%v 执行=%d", rerr, gate.n.Load())
	}
	st, step := roState(t, root, "星禾", "run-a")
	trs := roTransitions(t, root, "星禾", "run-a")
	aud := strings.Join(roAudits(t, root, "星禾", "run-a"), " | ")
	if st != run.StateFailed || step != run.StepError {
		t.Fatalf("state=%s step=%s", st, step)
	}
	if got := strings.Join(trs[3:], " | "); got != "awaiting_approval->running | running->failed" {
		t.Fatalf("批准后的迁移 %s", got)
	}
	if !strings.Contains(aud, run.AuditToolFailed) || strings.Contains(aud, run.AuditExecutingInterrupted) {
		t.Fatalf("audit %s", aud)
	}
	if got := roApproval(t, root, "星禾", "run-a"); !strings.Contains(got, "state=consumed") {
		t.Fatalf("approvals %s", got)
	}
	pending, err := run.ListPending(ctx, h.Runs, "星禾", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pending {
		if p.RunID == "run-a" {
			t.Fatal("已执行失败的票据又出现在待确认列表")
		}
	}
}

// 恢复路径一：进程外的残留（没有任何进程登记，最后一步停在 executing）在下一次 Ask 时
// 照常被标成 failed 并写 executing_interrupted，不重跑工具。
func TestAskStillRecoversUnheldExecutingResidual(t *testing.T) {
	ctx := t.Context()
	r, tool, w, root := newFlow(t, "星禾")
	askAndDefer(t, ctx, r, w, "星禾", "run-a")

	h, err := workspace.OpenPrepared(ctx, root, "星禾")
	if err != nil {
		t.Fatal(err)
	}
	tk, ok := pendingTicket(t, h, "run-a")
	if !ok {
		t.Fatal("没有待确认的票")
	}
	// 模拟别的进程已经开始执行后被硬杀：步骤落成 executing、运行 running，之后没人登记也没人收尾。
	crashed := run.New(h.Runs)
	if err := crashed.Begin(ctx, tk.StepID, "run-a", tk.Action, tk.Params); err != nil {
		t.Fatal(err)
	}
	if err := crashed.Set(ctx, "run-a", run.StateRunning); err != nil {
		t.Fatal(err)
	}
	_ = h.Close()

	askAndDefer(t, ctx, r, w, "星禾", "run-b")
	st, step := roState(t, root, "星禾", "run-a")
	aud := strings.Join(roAudits(t, root, "星禾", "run-a"), " | ")
	if st != run.StateFailed || step != run.StepExecuting {
		t.Fatalf("残留 state=%s step=%s", st, step)
	}
	if !strings.Contains(aud, run.AuditExecutingInterrupted) {
		t.Fatalf("audit %s", aud)
	}
	if tool.N() != 0 {
		t.Fatalf("恢复重跑了工具 %d 次", tool.N())
	}
}

// 恢复路径二：Resume 拿写锁时库被别的连接占着。锁在有界时间内释放，Resume 等到锁后执行一次，
// 中间同工作区的提问不会把它标成 failed。
func TestResumeWaitsOutLockedDatabase(t *testing.T) {
	ctx := t.Context()
	r, tool, w, root := newFlow(t, "星禾")
	askAndDefer(t, ctx, r, w, "星禾", "run-a")

	h, err := workspace.OpenPrepared(ctx, root, "星禾")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	locker, err := workspace.OpenPrepared(ctx, root, "星禾")
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close()
	svc := run.New(h.Runs)
	svc.SetApprover(w)
	svc.SetExecutor(tool.Execute)

	resc := make(chan error, 1)
	go func() {
		_, err := w.Resume(ctx, svc, "run-a")
		resc <- err
	}()
	tk := waitTicket(t, w, "run-a")
	conn, err := locker.Runs.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	if err := w.Deliver(tk.ID, approval.Decision{Action: approval.ActionApprove}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if tool.N() != 0 {
		t.Fatal("库被锁着时工具已经执行")
	}
	if _, err := conn.ExecContext(ctx, `ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	askAndDefer(t, ctx, r, w, "星禾", "run-b")
	select {
	case err := <-resc:
		if err != nil {
			t.Fatalf("Resume %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Resume 没有返回")
	}
	if tool.N() != 1 {
		t.Fatalf("执行 %d 次", tool.N())
	}
	trs := roTransitions(t, root, "星禾", "run-a")
	if got := strings.Join(trs[3:], " | "); got != "awaiting_approval->running | running->completed" {
		t.Fatalf("批准后的迁移 %s", got)
	}
	if aud := strings.Join(roAudits(t, root, "星禾", "run-a"), " | "); strings.Contains(aud, run.AuditExecutingInterrupted) || strings.Contains(aud, run.AuditIllegalResidual) {
		t.Fatalf("audit %s", aud)
	}
}

func pendingTicket(t *testing.T, h *workspace.Handle, runID string) (approval.Ticket, bool) {
	t.Helper()
	list, err := run.ListPending(t.Context(), h.Runs, h.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range list {
		if tk.RunID == runID {
			return tk, true
		}
	}
	return approval.Ticket{}, false
}
