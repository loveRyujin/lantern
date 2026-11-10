package ui

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	mygoui "github.com/egoist/mygo/ui"
	"github.com/loveRyujin/lantern/core/approval"
	"github.com/loveRyujin/lantern/core/events"
	"github.com/loveRyujin/lantern/core/workspace"
	"github.com/loveRyujin/lantern/internal/ui/session"
	_ "modernc.org/sqlite"
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

func TestEnterDefersSameTicketAndClickApprovesOnce(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	h, err := workspace.Create(ctx, root, "星禾")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	waiter := session.NewWaiter()
	runner := session.NewDemoRunner(root, waiter)
	s, err := newShell(ctx, Options{
		Root:       root,
		Workspaces: []session.Workspace{{ID: "星禾"}},
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
	askState, ticketID := askUntilDialog(t, tt, s, runner, "星禾", "导出客户")
	if tool.N() != 0 {
		t.Fatalf("弹窗出现时已经执行 %d 次", tool.N())
	}
	if !tt.Focused("暂缓") || tt.Focused("批准") {
		t.Fatal("Enter 的默认焦点不在暂缓上")
	}
	before := readApproval(t, root, "星禾", ticketID)
	tt.Key(0, mygoui.KeyEnter)
	tt.Frame()
	waitAskDone(t, tt, s, askState.RunID, session.StatusAwaiting)
	s.reloadList()
	tt.Frame()
	afterEnter := readApproval(t, root, "星禾", ticketID)
	if tool.N() != 0 || afterEnter != before || s.state.Phase() != "等待确认" {
		t.Fatalf("Enter 后 n=%d phase=%s before=%+v after=%+v", tool.N(), s.state.Phase(), before, afterEnter)
	}
	if !tt.HasText("重开 " + ticketID) {
		t.Fatalf("Enter 后原确认单不在列表：%q", tt.Texts())
	}

	reopenTicket(t, tt, s, ticketID)
	if !tt.Focused("暂缓") {
		t.Fatal("重开后默认焦点不在暂缓")
	}
	tt.Key(0, mygoui.KeyEscape)
	tt.Frame()
	waitNotice(t, tt, s, "仍在等待确认。")
	afterEscape := readApproval(t, root, "星禾", ticketID)
	if tool.N() != 0 || afterEscape != before {
		t.Fatalf("Escape 后 n=%d before=%+v after=%+v", tool.N(), before, afterEscape)
	}

	reopenTicket(t, tt, s, ticketID)
	if err := tt.Click("拒绝"); err != nil {
		t.Fatal(err)
	}
	waitNotice(t, tt, s, "已拒绝，工具未执行。")
	if tool.N() != 0 || s.state.Phase() != "已拒绝" {
		t.Fatalf("拒绝后 n=%d phase=%s", tool.N(), s.state.Phase())
	}
	// 拒绝已经消费这张票。再开一问，显式点击批准只能执行一次。
	askState, ticketID = askUntilDialog(t, tt, s, runner, "星禾", "再导出")
	if tool.N() != 0 || tt.Focused("批准") || !tt.Focused("暂缓") {
		t.Fatalf("第二张弹窗 n=%d focus暂缓=%v focus批准=%v", tool.N(), tt.Focused("暂缓"), tt.Focused("批准"))
	}
	if err := tt.Click("批准"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for tool.N() != 1 {
		tt.Frame()
		if time.Now().After(deadline) {
			t.Fatalf("点击批准后 n=%d phase=%s notice=%s", tool.N(), s.state.Phase(), shellNotice(s))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if tool.N() != 1 {
		t.Fatalf("执行了 %d 次", tool.N())
	}
	if n := countWorkspaceApprovals(t, root, "星禾"); n != 2 {
		t.Fatalf("确认单数量=%d", n)
	}
}

func askUntilDialog(t *testing.T, tt *mygoui.Tester, s *shell, runner *session.LoopRunner, ws, prompt string) (session.Ask, string) {
	t.Helper()
	askState, err := s.state.Begin(prompt)
	if err != nil {
		t.Fatal(err)
	}
	stream := events.NewStream(16)
	go func() {
		for range stream.Events() {
		}
	}()
	go func() {
		result, err := runner.Ask(s.ctx, session.Ask{
			RunID: askState.RunID, WorkspaceID: ws, Prompt: prompt, Events: stream,
		})
		stream.Close()
		s.onUI(func() {
			s.state.FinishResult(askState.RunID, err, result.RunState)
			s.reloadList()
		})
	}()
	deadline := time.Now().Add(5 * time.Second)
	var ticketID string
	for {
		tt.Frame()
		s.mu.Lock()
		if s.asking && s.dialog != nil && s.dialog.RunID == askState.RunID {
			ticketID = s.dialog.ID
		}
		s.mu.Unlock()
		if ticketID != "" && tt.HasText("将执行 export_data") && tt.HasText("**不要执行**") && tt.Focused("暂缓") {
			return askState, ticketID
		}
		if time.Now().After(deadline) {
			t.Fatalf("确认单没有出现：%q focused暂缓=%v", tt.Texts(), tt.Focused("暂缓"))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitAskDone(t *testing.T, tt *mygoui.Tester, s *shell, runID, state string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		tt.Frame()
		msg := s.state.Thread(s.state.ActiveID())
		for i := range msg.Messages {
			m := &msg.Messages[i]
			if m.RunID == runID && m.Done && m.Status == state {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("运行 %s 没有变成 %s", runID, state)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func reopenTicket(t *testing.T, tt *mygoui.Tester, s *shell, ticketID string) {
	t.Helper()
	tt.Frame()
	if err := tt.Click("重开 " + ticketID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		tt.Frame()
		s.mu.Lock()
		again := s.asking && s.dialog != nil && s.dialog.ID == ticketID
		s.mu.Unlock()
		if again && tt.Focused("暂缓") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("重开没有回到同一张确认单")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitNotice(t *testing.T, tt *mygoui.Tester, s *shell, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		tt.Frame()
		if shellNotice(s) == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("notice=%q want %q phase=%s", shellNotice(s), want, s.state.Phase())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type approvalRow struct {
	id, state string
	version   int64
}

func readApproval(t *testing.T, root, id, ticketID string) approvalRow {
	t.Helper()
	db := openRunsRO(t, root, id)
	defer db.Close()
	var row approvalRow
	if err := db.QueryRowContext(t.Context(), `SELECT id, state, version FROM approvals WHERE id = ?`, ticketID).Scan(&row.id, &row.state, &row.version); err != nil {
		t.Fatal(err)
	}
	return row
}

func TestOldSchemaStartsListsAndReopensSameTicket(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	const (
		ticketID = "ap-legacy"
		runID    = "run-legacy"
		ws       = "星禾"
		other    = "云杉"
	)
	params := []byte(`{"object":"旧库导出"}`)
	sum, err := approval.ParamsHash(params)
	if err != nil {
		t.Fatal(err)
	}
	seedOldWorkspace(t, root, ws, &legacyTicket{
		ID: ticketID, RunID: runID, StepID: "step-legacy",
		Params: params, Hash: sum, Action: "export_data",
	})
	seedOldWorkspace(t, root, other, nil)
	if riskColumn(t, root, ws) {
		t.Fatal("旧库在启动前就有 risk 列")
	}
	if n := migrationCount(t, root, ws); n != 4 {
		t.Fatalf("旧库迁移数=%d", n)
	}

	waiter := session.NewWaiter()
	runner := session.NewDemoRunner(root, waiter)
	s, err := newShell(ctx, Options{Root: root, Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	tool, ok := runner.Tool.(*session.CountTool)
	if !ok {
		t.Fatal("演示工具不是计数器")
	}
	if !riskColumn(t, root, ws) || migrationCount(t, root, ws) != 5 {
		t.Fatalf("启动没有补上 risk 迁移 risk=%v n=%d", riskColumn(t, root, ws), migrationCount(t, root, ws))
	}
	if !s.state.Switch(ws) {
		t.Fatal("没有星禾")
	}
	s.reloadList()
	s.reloadList()
	got := shellList(s)
	if shellNotice(s) != "" || len(got) != 1 || got[0].ID != ticketID || got[0].RunID != runID || got[0].Risk != "" || string(got[0].Params) != string(params) {
		t.Fatalf("list=%+v notice=%s", got, shellNotice(s))
	}
	if session.RiskLine(got[0].Risk) != session.RiskUnfrozen {
		t.Fatalf("risk line=%s", session.RiskLine(got[0].Risk))
	}
	if restoredAudits(t, root, ws) != 0 || restoredAudits(t, root, other) != 0 {
		t.Fatal("首次列出写了 approval_restored")
	}

	again, err := newShell(ctx, Options{Root: root, Runner: session.NewDemoRunner(root, session.NewWaiter())})
	if err != nil {
		t.Fatal(err)
	}
	if !again.state.Switch(ws) {
		t.Fatal("第二次启动没有星禾")
	}
	again.reloadList()
	second := shellList(again)
	if len(second) != 1 || second[0].ID != ticketID || restoredAudits(t, root, ws) != 0 {
		t.Fatalf("再次打开 list=%+v audits=%d", second, restoredAudits(t, root, ws))
	}
	if n := approvalCount(t, root, ws); n != 1 {
		t.Fatalf("再次打开后票据数=%d", n)
	}

	if !s.state.Switch(other) {
		t.Fatal("没有云杉")
	}
	s.reloadList()
	if otherList := shellList(s); len(otherList) != 0 || shellNotice(s) != "" {
		t.Fatalf("云杉看见了星禾的票据 %+v notice=%s", otherList, shellNotice(s))
	}
	s.reopen(got[0])
	if tool.N() != 0 || waiter.Busy(runID) {
		t.Fatal("在云杉重开了星禾的确认单")
	}
	if !s.state.Switch(ws) {
		t.Fatal("切不回星禾")
	}
	s.reloadList()

	tt := tester(s)
	tt.Frame()
	if !tt.HasText("重开 " + ticketID) {
		t.Fatalf("列表没有原确认单：%q", tt.Texts())
	}
	s.reopen(got[0])
	deadline := time.Now().Add(5 * time.Second)
	for {
		tt.Frame()
		s.mu.Lock()
		shown := s.asking && s.dialog != nil && s.dialog.ID == ticketID && s.dialog.Risk == ""
		s.mu.Unlock()
		if shown && tt.HasText(session.RiskUnfrozen) && tt.HasText("旧库导出") && tool.N() == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("重开没有展示原票据 phase=%s n=%d texts=%q", s.state.Phase(), tool.N(), tt.Texts())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := waiter.Deliver(ticketID, approval.Decision{Action: approval.ActionApprove}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		tt.Frame()
		if tool.N() == 1 && shellNotice(s) == "已按冻结参数执行一次。没有继续问模型。" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("批准后 phase=%s n=%d notice=%s", s.state.Phase(), tool.N(), shellNotice(s))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if tool.N() != 1 || approvalCount(t, root, ws) != 1 || approvalCount(t, root, other) != 0 {
		t.Fatalf("n=%d 星禾票据=%d 云杉票据=%d", tool.N(), approvalCount(t, root, ws), approvalCount(t, root, other))
	}
	if id, risk := approvalRisk(t, root, ws); id != ticketID || risk != "" {
		t.Fatalf("票据=%s risk=%q", id, risk)
	}
	if restoredAudits(t, root, ws) != 0 || restoredAudits(t, root, other) != 0 {
		t.Fatal("重开或刷新追加了 approval_restored")
	}
	s.reloadList()
	if left := shellList(s); len(left) != 0 || restoredAudits(t, root, ws) != 0 {
		t.Fatalf("完成后仍待确认 %+v audits=%d", left, restoredAudits(t, root, ws))
	}
}

type legacyTicket struct {
	ID, RunID, StepID, Hash, Action string
	Params                          []byte
}

func seedOldWorkspace(t *testing.T, root, id string, ticket *legacyTicket) {
	t.Helper()
	dir := filepath.Join(root, "workspaces", id)
	for _, sub := range []string{"", "exports", "tmp"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"kb.db", "analytics.db"} {
		touchSQLite(t, filepath.Join(dir, name))
	}
	db := touchSQLite(t, filepath.Join(dir, "runs.db"))
	applyRepoMigrations(t, db, "runs", []string{
		"001_init.sql", "002_export_approval.sql", "003_approval_state.sql", "004_audit_run_id.sql",
	})
	if ticket == nil {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO runs (id, state, version, context, created_at, updated_at)
		VALUES (?, 'awaiting_approval', 0, '', ?, ?)`, ticket.RunID, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO approvals (
			id, run_id, step_id, params_hash, params_json, decision, created_at,
			workspace_id, action, export_object, expires_at, state, version
		) VALUES (?, ?, ?, ?, ?, '', ?, ?, ?, '', ?, 'pending', 0)`,
		ticket.ID, ticket.RunID, ticket.StepID, ticket.Hash, string(ticket.Params),
		now, id, ticket.Action, exp); err != nil {
		t.Fatal(err)
	}
}

func touchSQLite(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	return db
}

func applyRepoMigrations(t *testing.T, db *sql.DB, kind string, names []string) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			applied_at TEXT NOT NULL
		)`); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join("..", "..", "core", "workspace", "migrations", kind)
	for _, name := range names {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		version := migrationPrefix(t, name)
		tx, err := db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), string(body)); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(),
			`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
			version, name, "2026-01-01T00:00:00Z"); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
}

func migrationPrefix(t *testing.T, name string) int {
	t.Helper()
	i := 0
	for i < len(name) && name[i] >= '0' && name[i] <= '9' {
		i++
	}
	n, err := strconv.Atoi(name[:i])
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func riskColumn(t *testing.T, root, id string) bool {
	t.Helper()
	db := openRunsRO(t, root, id)
	defer db.Close()
	var n int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM pragma_table_info('approvals') WHERE name = 'risk'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func migrationCount(t *testing.T, root, id string) int {
	t.Helper()
	db := openRunsRO(t, root, id)
	defer db.Close()
	var n int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func restoredAudits(t *testing.T, root, id string) int {
	t.Helper()
	db := openRunsRO(t, root, id)
	defer db.Close()
	var n int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM audit WHERE action = 'approval_restored'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func approvalCount(t *testing.T, root, id string) int {
	t.Helper()
	db := openRunsRO(t, root, id)
	defer db.Close()
	var n int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM approvals`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func approvalRisk(t *testing.T, root, id string) (string, string) {
	t.Helper()
	db := openRunsRO(t, root, id)
	defer db.Close()
	var ticketID, risk string
	if err := db.QueryRowContext(t.Context(), `SELECT id, risk FROM approvals`).Scan(&ticketID, &risk); err != nil {
		t.Fatal(err)
	}
	return ticketID, risk
}

func openRunsRO(t *testing.T, root, id string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(root, "workspaces", id, "runs.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func shellList(s *shell) []approval.Ticket {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]approval.Ticket(nil), s.list...)
}

func shellNotice(s *shell) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.notice
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
