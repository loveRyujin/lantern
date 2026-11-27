package ui

import (
	"errors"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/approval"
	"github.com/loveRyujin/lantern/core/events"
	"github.com/loveRyujin/lantern/core/run"
	"github.com/loveRyujin/lantern/core/workspace"
	"github.com/loveRyujin/lantern/internal/ui/session"
)

// TestShellTraceDuringResumeShowsExecuting 走和 TestShellAskDuringResumeKeepsExecutingRun 相同的桌面路径，
// 检查界面上的字：工具执行中、第二问之后，轨迹页显示运行中 / 执行中，不出现失败或执行中断；
// Resume 返回后，轨迹页、通知和会话消息都是已完成。
func TestShellTraceDuringResumeShowsExecuting(t *testing.T) {
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
	tt.Frame()
	if err := tt.Click("暂缓"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	s.reloadList()
	list := shellList(s)
	if len(list) != 1 {
		t.Fatalf("待确认列表=%+v", list)
	}
	tk := list[0]
	runA := tk.RunID

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
	tt.Frame()
	ask(t, tt, "顺便看看上个月营收")
	tt.Frame()

	s.openTrace(runA)
	tt.Frame()
	if !tt.HasText("运行 "+runA+" · 状态 运行中") || !tt.HasText("工具 export_data · 执行中") {
		t.Fatalf("执行中的轨迹页没有显示运行中 / 执行中：%+v %+v", s.trace.header, s.trace.lines)
	}
	for _, bad := range []string{"状态 失败", "执行中断", run.AuditExecutingInterrupted} {
		if tt.HasText(bad) {
			t.Errorf("执行中的轨迹页出现 %q", bad)
		}
	}

	close(gate.release)
	raceWait(t, tt, "Resume 收尾", func() bool {
		return shellNotice(s) == "已按冻结参数执行一次。没有继续问模型。"
	})
	s.loadTrace(runA)
	tt.Frame()
	if !tt.HasText("运行 "+runA+" · 状态 已完成") || !tt.HasText("工具 export_data · 完成") {
		t.Fatalf("Resume 返回后轨迹页 %+v %+v", s.trace.header, s.trace.lines)
	}
	for _, bad := range []string{"状态 失败", "执行中断"} {
		if tt.HasText(bad) {
			t.Errorf("Resume 返回后轨迹页出现 %q", bad)
		}
	}
	var msg *session.Message
	for i, m := range s.state.Thread(ws).Messages {
		if m.RunID == runA {
			msg = &s.state.Thread(ws).Messages[i]
		}
	}
	if msg == nil || !msg.Done || msg.Status != events.DoneCompleted || msg.Note != "已按冻结参数执行一次。没有继续问模型。" {
		t.Fatalf("会话消息 %+v", msg)
	}
	if gate.n.Load() != 1 {
		t.Fatalf("执行 %d 次", gate.n.Load())
	}
}
