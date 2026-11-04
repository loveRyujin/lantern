package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/loveRyujin/lantern/core/events"
)

func TestBeginSwitchAndRejectsSecond(t *testing.T) {
	st := mustState(t, "星禾", "云杉")
	if st.ActiveID() != "星禾" || st.Phase() != "等待提问" {
		t.Fatalf("active=%s phase=%s", st.ActiveID(), st.Phase())
	}
	if !st.Switch("云杉") || st.ActiveID() != "云杉" {
		t.Fatalf("switch failed, active=%s", st.ActiveID())
	}
	if st.Switch("不存在") || st.ActiveID() != "云杉" {
		t.Fatalf("unknown switch changed active to %s", st.ActiveID())
	}
	ask, err := st.Begin("  上个月营收  ")
	if err != nil {
		t.Fatal(err)
	}
	if ask.WorkspaceID != "云杉" || ask.Prompt != "上个月营收" || st.Draft != "" {
		t.Fatalf("ask=%+v draft=%q", ask, st.Draft)
	}
	if st.Phase() != "流式中" {
		t.Fatalf("phase=%s", st.Phase())
	}
	if _, err := st.Begin("再问"); err == nil {
		t.Fatal("second begin succeeded while streaming")
	}
	user := st.Thread("云杉").Messages
	if len(user) != 2 || user[0].Role != RoleUser || user[0].Text != "上个月营收" || user[1].RunID != ask.RunID {
		t.Fatalf("messages=%+v", user)
	}
	if len(st.Thread("星禾").Messages) != 0 {
		t.Fatal("星禾 received 云杉's question")
	}
}

func TestTokenStepAndDoneDriveText(t *testing.T) {
	st := mustState(t, "星禾")
	ask, err := st.Begin("营收")
	if err != nil {
		t.Fatal(err)
	}
	st.Apply(events.Event{
		Kind:  events.KindStep,
		RunID: ask.RunID,
		Step:  &events.Step{ID: "compose", Status: events.StepExecuting, Summary: "整理回答"},
	})
	st.Apply(events.Event{
		Kind:  events.KindToken,
		RunID: ask.RunID,
		Token: &events.Token{Text: "营收按**已确"},
	})
	msg := st.Thread("星禾").Messages[1]
	if msg.Done || msg.Text != "营收按**已确" || msg.Steps[0].Line() != "进行中 · 整理回答" {
		t.Fatalf("partial=%+v", msg)
	}
	st.Apply(events.Event{
		Kind:  events.KindToken,
		RunID: ask.RunID,
		Token: &events.Token{Text: "认**订单。"},
	})
	st.Apply(events.Event{
		Kind:  events.KindStep,
		RunID: ask.RunID,
		Step:  &events.Step{ID: "compose", Status: events.StepDone, Summary: "整理回答"},
	})
	st.Apply(events.Event{
		Kind:  events.KindApprovalRequest,
		RunID: ask.RunID,
		Approval: &events.ApprovalRequest{
			TicketID: "ap-1",
			Action:   "export_data",
		},
	})
	st.Apply(events.Event{
		Kind:  events.KindDone,
		RunID: ask.RunID,
		Done:  &events.Done{Status: events.DoneCompleted, Text: "这段不该覆盖已经流过的 token"},
	})
	msg = st.Thread("星禾").Messages[1]
	if !msg.Done || msg.Status != events.DoneCompleted || st.Phase() != "已完成" {
		t.Fatalf("done msg=%+v phase=%s", msg, st.Phase())
	}
	if msg.Text != "营收按**已确认**订单。" {
		t.Fatalf("tokens were replaced: %q", msg.Text)
	}
	if len(msg.Steps) != 1 || msg.Steps[0].Line() != "完成 · 整理回答" {
		t.Fatalf("steps=%+v", msg.Steps)
	}
	if !strings.Contains(msg.Approval, "本张不弹窗") || !strings.Contains(msg.Approval, "export_data") {
		t.Fatalf("approval=%q", msg.Approval)
	}
	before := st.Updates
	st.Apply(events.Event{Kind: events.KindToken, RunID: ask.RunID, Token: &events.Token{Text: "晚到"}})
	msg = st.Thread("星禾").Messages[1]
	if msg.Text != "营收按**已确认**订单。" || st.Updates != before {
		t.Fatalf("late token applied: %q updates %d -> %d", msg.Text, before, st.Updates)
	}
}

func TestDoneWithoutTokensUsesFinalText(t *testing.T) {
	st := mustState(t, "星禾")
	ask, err := st.Begin("只看终态")
	if err != nil {
		t.Fatal(err)
	}
	st.Apply(events.Event{
		Kind:  events.KindStep,
		RunID: ask.RunID,
		Step: &events.Step{
			ID:       "sql-1",
			Status:   events.StepDone,
			Summary:  "只读查询",
			ToolName: "run_sql",
		},
	})
	const answer = "营收按已确认订单统计。\n\n```sql\nSELECT month FROM analytics\n```\n"
	st.Apply(events.Event{
		Kind:  events.KindDone,
		RunID: ask.RunID,
		Done:  &events.Done{Status: events.DoneCompleted, Text: answer},
	})
	msg := st.Thread("星禾").Messages[1]
	if msg.Text != answer || msg.Steps[0].Line() != "完成 · run_sql · 只读查询" {
		t.Fatalf("loop-shaped result=%+v", msg)
	}
}

func TestFinishWithoutDoneMarksFailed(t *testing.T) {
	st := mustState(t, "星禾")
	ask, err := st.Begin("会失败")
	if err != nil {
		t.Fatal(err)
	}
	st.Finish(ask.RunID, errors.New("session: 演示失败"))
	msg := st.Thread("星禾").Messages[1]
	if !msg.Done || msg.Status != events.DoneFailed || msg.Note != "session: 演示失败" || st.Phase() != "失败" {
		t.Fatalf("finish=%+v phase=%s", msg, st.Phase())
	}
}

func TestApplyDropsInvalid(t *testing.T) {
	st := mustState(t, "星禾")
	ask, err := st.Begin("q")
	if err != nil {
		t.Fatal(err)
	}
	st.Apply(events.Event{Kind: events.KindToken, RunID: ask.RunID})
	if st.Updates != 0 || st.Thread("星禾").Messages[1].Text != "" {
		t.Fatal("invalid token was applied")
	}
}

func TestScriptedRunnerPublishesTokenStream(t *testing.T) {
	ctx := t.Context()
	stream := events.NewStream(4)
	errc := make(chan error, 1)
	go func() {
		errc <- (ScriptedRunner{Chunk: 1}).Ask(ctx, Ask{
			RunID:       "run-1",
			WorkspaceID: "星禾",
			Prompt:      "营收",
			Events:      stream,
		})
		stream.Close()
	}()
	var got []events.Event
	for ev := range stream.Events() {
		got = append(got, ev)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	answer := defaultAnswer("星禾", "营收")
	tokens := 0
	var b strings.Builder
	sawExec, sawDoneStep, sawComplete := false, false, false
	for _, ev := range got {
		if err := ev.Validate(); err != nil {
			t.Fatalf("event %+v: %v", ev.Kind, err)
		}
		switch ev.Kind {
		case events.KindToken:
			tokens++
			b.WriteString(ev.Token.Text)
		case events.KindStep:
			if ev.Step.Status == events.StepExecuting {
				sawExec = true
			}
			if ev.Step.Status == events.StepDone {
				sawDoneStep = true
			}
		case events.KindDone:
			if ev.Done.Status == events.DoneCompleted && ev.Done.Text == answer {
				sawComplete = true
			}
		}
	}
	if tokens != utf8.RuneCountInString(answer) || b.String() != answer {
		t.Fatalf("tokens=%d text=%q", tokens, b.String())
	}
	if !sawExec || !sawDoneStep || !sawComplete {
		t.Fatalf("exec=%v step=%v done=%v kinds=%d", sawExec, sawDoneStep, sawComplete, len(got))
	}
	if got[0].Kind != events.KindStep || got[len(got)-1].Kind != events.KindDone {
		t.Fatalf("ends: %s ... %s", got[0].Kind, got[len(got)-1].Kind)
	}
}

func TestScriptedRunnerStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	stream := events.NewStream(1)
	t.Cleanup(stream.Close)
	err := (ScriptedRunner{}).Ask(ctx, Ask{
		RunID:  "run",
		Events: stream,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func mustState(t *testing.T, ids ...string) *State {
	t.Helper()
	ws := make([]Workspace, len(ids))
	for i, id := range ids {
		ws[i] = Workspace{ID: id}
	}
	st, err := New(ws)
	if err != nil {
		t.Fatal(err)
	}
	return st
}
