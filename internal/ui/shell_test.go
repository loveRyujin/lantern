package ui

import (
	"context"
	"errors"
	"strings"
	"testing"

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
	if !tt.HasText("本张不弹窗") || !tt.HasText("export_data") {
		t.Fatalf("确认请求没有留在对话里：%q", tt.Texts())
	}
	if tt.HasText("暂缓") || tt.HasText("批准") {
		t.Fatalf("出现了确认框按钮：%q", tt.Texts())
	}
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

func (failRunner) Ask(context.Context, session.Ask) error {
	return errors.New("ui: 演示失败")
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
