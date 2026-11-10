package ui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	mygoui "github.com/egoist/mygo/ui"
	"github.com/loveRyujin/lantern/core/events"
	"github.com/loveRyujin/lantern/core/workspace"
	"github.com/loveRyujin/lantern/internal/ui/session"
)

func TestSidebarSwitchesWorkspace(t *testing.T) {
	s := testShell(t, session.ScriptedRunner{}, "星禾", "云杉")
	tt := tester(s)
	tt.Frame()
	for _, want := range []string{"工作区", "星禾", "云杉", "当前会话", "本地演示回答，不联网。", "等待提问"} {
		if !tt.HasText(want) {
			t.Fatalf("缺少 %q，现有 %q", want, tt.Texts())
		}
	}
	if err := tt.Click("云杉"); err != nil {
		t.Fatal(err)
	}
	if s.state.ActiveID() != "云杉" {
		t.Fatalf("active=%s", s.state.ActiveID())
	}
}

func TestSubmitStreamsMarkdownFromCoreEvents(t *testing.T) {
	s := testShell(t, session.ScriptedRunner{}, "星禾", "云杉")
	tt := tester(s)
	tt.Frame()
	if err := tt.Click("云杉"); err != nil {
		t.Fatal(err)
	}
	ask(t, tt, "上个月营收")
	tt.Frame()
	for _, want := range []string{"上个月营收", "已收到", "已确认", "8 月", "96", "SELECT", "revenue", "已完成", "整理回答"} {
		if !tt.HasText(want) {
			t.Errorf("完成后缺少 %q，现有 %q", want, tt.Texts())
		}
	}
	if tt.HasText("**") {
		t.Fatalf("标记漏进了画面：%q", tt.Texts())
	}
	if s.state.Thread("星禾").Messages != nil && len(s.state.Thread("星禾").Messages) != 0 {
		t.Fatal("星禾 出现了 云杉 的对话")
	}
	msg := s.state.Thread("云杉").Messages
	if len(msg) != 2 || !strings.Contains(msg[1].Text, "SELECT") || msg[1].Status != events.DoneCompleted {
		t.Fatalf("云杉 messages=%+v", msg)
	}
	if s.state.Updates < 3 {
		t.Fatalf("updates=%d，事件没有驱动状态", s.state.Updates)
	}
	if err := tt.Click("星禾"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if tt.HasText("SELECT") || tt.HasText("上个月营收") {
		t.Fatalf("切走之后还能看见上一区的回答：%q", tt.Texts())
	}
	if err := tt.Click("云杉"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if !tt.HasText("SELECT") {
		t.Fatal("切回来后回答不见了")
	}
}

func TestPartialTokenUsesMarkdownStream(t *testing.T) {
	s := testShell(t, session.ScriptedRunner{}, "星禾")
	tt := tester(s)
	tt.Frame()
	askState, err := s.state.Begin("营收")
	if err != nil {
		t.Fatal(err)
	}
	s.state.Apply(events.Event{
		Kind:  events.KindToken,
		RunID: askState.RunID,
		Token: &events.Token{Text: "营收按**已确"},
	})
	tt.Frame()
	if !tt.HasText("营收按") || !tt.HasText("已确") {
		t.Fatalf("流式文本没有画出来：%q", tt.Texts())
	}
	if tt.HasText("**") || tt.HasText("SELECT") || tt.HasText("8 月") {
		t.Fatalf("未闭合标记或后文漏进了画面：%q", tt.Texts())
	}
	s.state.Apply(events.Event{
		Kind:  events.KindToken,
		RunID: askState.RunID,
		Token: &events.Token{Text: "认**订单统计。\n\n```sql\nSELECT 1\n```\n"},
	})
	s.state.Apply(events.Event{
		Kind:  events.KindDone,
		RunID: askState.RunID,
		Done:  &events.Done{Status: events.DoneCompleted, Text: "ignored"},
	})
	tt.Frame()
	if !tt.HasText("SELECT") || !tt.HasText("订单统计") {
		t.Fatalf("完成后缺少正文：%q", tt.Texts())
	}
	if tt.HasText("**") {
		t.Fatalf("完成后仍有星号：%q", tt.Texts())
	}
}

func TestLoopShapedEventsRender(t *testing.T) {
	s := testShell(t, session.ScriptedRunner{}, "星禾")
	tt := tester(s)
	askState, err := s.state.Begin("只看终态")
	if err != nil {
		t.Fatal(err)
	}
	s.state.Apply(events.Event{
		Kind:  events.KindStep,
		RunID: askState.RunID,
		Step: &events.Step{
			ID: "sql-1", Status: events.StepDone, Summary: "只读查询", ToolName: "run_sql",
		},
	})
	s.state.Apply(events.Event{
		Kind:  events.KindDone,
		RunID: askState.RunID,
		Done:  &events.Done{Status: events.DoneCompleted, Text: "结论见下。\n\n```sql\nSELECT month FROM analytics\n```\n"},
	})
	tt.Frame()
	for _, want := range []string{"run_sql", "只读查询", "SELECT", "month"} {
		if !tt.HasText(want) {
			t.Errorf("缺少 %q，现有 %q", want, tt.Texts())
		}
	}
}

func TestApprovalStaysInline(t *testing.T) {
	s := testShell(t, session.ScriptedRunner{}, "星禾")
	tt := tester(s)
	askState, err := s.state.Begin("导出")
	if err != nil {
		t.Fatal(err)
	}
	s.state.Apply(events.Event{
		Kind:  events.KindApprovalRequest,
		RunID: askState.RunID,
		Approval: &events.ApprovalRequest{
			TicketID: "ap-1",
			Action:   "export_data",
		},
	})
	tt.Frame()
	if !tt.HasText("等待确认") || !tt.HasText("export_data") {
		t.Fatalf("确认请求没有留在对话里：%q", tt.Texts())
	}
	if tt.HasText("暂缓") || tt.HasText("批准") {
		t.Fatalf("出现了确认框按钮：%q", tt.Texts())
	}
}

func TestDialogShowsFrozenTicketAndReopens(t *testing.T) {
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
	waiter := session.NewWaiter()
	runner := session.NewDemoRunner(root, waiter)
	s, err := newShell(ctx, Options{
		Root:       root,
		Workspaces: []session.Workspace{{ID: "星禾"}, {ID: "云杉"}},
		Runner:     runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	tool, ok := runner.Tool.(*session.CountTool)
	if !ok {
		t.Fatal("演示工具不是计数器")
	}
	tt := tester(s)
	tt.Frame()
	if !tt.HasText("演示：确定性模型，经真实 Agent 循环。确认单来自 Core，不联网。") {
		t.Fatalf("没有标明演示：%q", tt.Texts())
	}
	askState, err := s.state.Begin("导出客户")
	if err != nil {
		t.Fatal(err)
	}
	stream := events.NewStream(16)
	evc := make(chan events.Event, 16)
	go func() {
		for ev := range stream.Events() {
			evc <- ev
		}
		close(evc)
	}()
	done := make(chan error, 1)
	go func() {
		result, err := runner.Ask(ctx, session.Ask{
			RunID: askState.RunID, WorkspaceID: "星禾", Prompt: "导出客户", Events: stream,
		})
		stream.Close()
		if err == nil && result.RunState != session.StatusAwaiting {
			err = errors.New("run state " + result.RunState)
		}
		done <- err
	}()

	var ticketID, hash string
	deadline := time.Now().Add(5 * time.Second)
	for {
		tt.Frame()
		drainUIEvents(s, evc)
		s.mu.Lock()
		if s.asking && s.dialog != nil {
			ticketID = s.dialog.ID
			hash = s.dialog.ParamsHash
		}
		s.mu.Unlock()
		if ticketID != "" && tt.HasText("将执行 export_data") && tt.HasText("**不要执行**") && tt.HasText("DROP TABLE users") && tt.HasText(hash) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("确认单没有按纯文本出现：%q", tt.Texts())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if tool.N() != 0 {
		t.Fatalf("弹窗出现时已经执行 %d 次", tool.N())
	}
	tt.Key(0, mygoui.KeyEscape)
	tt.Frame()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("暂缓后提问没有返回")
	}
	s.state.FinishResult(askState.RunID, nil, session.StatusAwaiting)
	s.reloadList()
	tt.Frame()
	if tool.N() != 0 || s.state.Phase() != "等待确认" {
		t.Fatalf("phase=%s n=%d", s.state.Phase(), tool.N())
	}
	if !tt.HasText("重开 " + ticketID) {
		t.Fatalf("列表没有原确认单：%q", tt.Texts())
	}
	if err := tt.Click("云杉"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if tt.HasText(ticketID) || tt.HasText("DROP TABLE users") || tt.HasText("**不要执行**") || tt.HasText("将执行 export_data") {
		t.Fatalf("云杉看见了星禾的确认单：%q", tt.Texts())
	}
	if err := tt.Click("星禾"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	list, err := session.ReadPending(ctx, root, "星禾", time.Now())
	if err != nil || len(list) != 1 || list[0].ID != ticketID {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	if err := tt.Click("重开 " + ticketID); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		tt.Frame()
		s.mu.Lock()
		again := s.asking && s.dialog != nil && s.dialog.ID == ticketID
		s.mu.Unlock()
		if again && tt.HasText("拒绝") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("重开没有回到同一张确认单")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := tt.Click("拒绝"); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		tt.Frame()
		if s.state.Phase() == "已拒绝" && tool.N() == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("phase=%s n=%d texts=%q", s.state.Phase(), tool.N(), tt.Texts())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := countWorkspaceApprovals(t, root, "星禾"); n != 1 {
		t.Fatalf("重开后确认单数量=%d", n)
	}
	left, err := session.ReadPending(ctx, root, "星禾", time.Now())
	if err != nil || len(left) != 0 {
		t.Fatalf("拒绝后仍待确认 %+v err=%v", left, err)
	}
}

func drainUIEvents(s *shell, evc <-chan events.Event) {
	for {
		select {
		case ev, ok := <-evc:
			if !ok {
				return
			}
			s.state.Apply(ev)
		default:
			return
		}
	}
}

func countWorkspaceApprovals(t *testing.T, root, id string) int {
	t.Helper()
	h, err := workspace.Open(t.Context(), root, id)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	var n int
	if err := h.Runs.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM approvals`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestEmptySendDoesNothing(t *testing.T) {
	s := testShell(t, session.ScriptedRunner{}, "星禾")
	tt := tester(s)
	tt.Frame()
	if err := tt.Click("发送"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if len(s.state.Thread("星禾").Messages) != 0 || s.state.Phase() != "等待提问" {
		t.Fatalf("空提问被发出去了 phase=%s messages=%d", s.state.Phase(), len(s.state.Thread("星禾").Messages))
	}
}

func TestFailedRunShowsNote(t *testing.T) {
	s := testShell(t, failRunner{}, "星禾")
	tt := tester(s)
	tt.Frame()
	ask(t, tt, "会失败")
	tt.Frame()
	if !tt.HasText("这次运行失败。") || !tt.HasText("演示失败") || s.state.Phase() != "失败" {
		t.Fatalf("phase=%s texts=%q", s.state.Phase(), tt.Texts())
	}
}

func TestLoadWorkspacesCreatesDemoAndKeepsExisting(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	got, err := loadWorkspaces(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "云杉" || got[1].ID != "星禾" {
		t.Fatalf("demo=%+v", got)
	}
	again, err := loadWorkspaces(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 2 {
		t.Fatalf("second load=%+v", again)
	}

	other := t.TempDir()
	h, err := workspace.Create(ctx, other, "only")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	kept, err := loadWorkspaces(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 || kept[0].ID != "only" {
		t.Fatalf("kept=%+v", kept)
	}
}

type failRunner struct{}

func (failRunner) Ask(context.Context, session.Ask) (session.AskResult, error) {
	return session.AskResult{}, errors.New("ui: 演示失败")
}

func testShell(t *testing.T, runner session.Runner, ids ...string) *shell {
	t.Helper()
	ws := make([]session.Workspace, len(ids))
	for i, id := range ids {
		ws[i] = session.Workspace{ID: id}
	}
	s, err := newShell(t.Context(), Options{Workspaces: ws, Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func tester(s *shell) *mygoui.Tester {
	return mygoui.NewTester(s.view, 1080, 720)
}

func ask(t *testing.T, tt *mygoui.Tester, prompt string) {
	t.Helper()
	if err := tt.Click("提问框"); err != nil {
		t.Fatal(err)
	}
	tt.Type(prompt)
	tt.Frame()
	if err := tt.Click("发送"); err != nil {
		t.Fatal(err)
	}
}
