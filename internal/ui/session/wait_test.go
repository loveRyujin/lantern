package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/approval"
)

func waitTestTicket(id, runID string) approval.Ticket {
	params := []byte(`{"object":"客户"}`)
	sum, err := approval.ParamsHash(params)
	if err != nil {
		panic(err)
	}
	return approval.Ticket{
		ID: id, RunID: runID, StepID: "step-" + runID, WorkspaceID: "星禾",
		Action: "export_data", Params: params, ParamsHash: sum,
		ExpiresAt: time.Now().Add(time.Hour),
	}
}

func waitLive(t *testing.T, w *Waiter, runID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !w.Busy(runID) {
		if time.Now().After(deadline) {
			t.Fatal("Request 没有登记")
		}
		time.Sleep(time.Millisecond)
	}
}

// 退出时先 Shutdown 送暂缓、紧接着取消 ctx。Request 还没进 select 时两者已同时就绪（窗口里 onShow 正等主线程，
// 用户在这一帧点了「仍退出」），也必须拿到暂缓，否则运行会落成 cancelled、票据丢失。
func TestShutdownThenCancelAlwaysDefers(t *testing.T) {
	for i := 0; i < 200; i++ {
		w := NewWaiter()
		ctx, cancel := context.WithCancel(context.Background())
		var deferred []approval.Ticket
		w.SetOnShow(func(approval.Ticket) {
			deferred = w.Shutdown()
			cancel()
		})
		d, err := w.Request(ctx, waitTestTicket("ap-1", "run-1"))
		if err != nil || d.Action != approval.ActionDefer {
			t.Fatalf("第 %d 次 err=%v action=%s", i, err, d.Action)
		}
		if len(deferred) != 1 || deferred[0].ID != "ap-1" {
			t.Fatalf("deferred=%+v", deferred)
		}
	}
}

// 已经在 select 里等着的 Request 同样拿到暂缓。
func TestShutdownWhileWaitingDefers(t *testing.T) {
	w := NewWaiter()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan approval.Decision, 1)
	go func() {
		d, _ := w.Request(ctx, waitTestTicket("ap-1", "run-1"))
		got <- d
	}()
	waitLive(t, w, "run-1")
	w.Shutdown()
	cancel()
	if d := <-got; d.Action != approval.ActionDefer {
		t.Fatalf("action=%s", d.Action)
	}
}

func TestShutdownDefersLaterRequestsAndRefusesResume(t *testing.T) {
	w := NewWaiter()
	if a := w.Activity(); len(a.Waiting) != 0 || len(a.Resuming) != 0 {
		t.Fatalf("空等待器 activity=%+v", a)
	}
	shown := 0
	w.SetOnShow(func(approval.Ticket) { shown++ })
	w.Shutdown()
	d, err := w.Request(t.Context(), waitTestTicket("ap-2", "run-2"))
	if err != nil || d.Action != approval.ActionDefer || shown != 0 || w.Busy("run-2") {
		t.Fatalf("退出后到达的 Request d=%+v err=%v shown=%d", d, err, shown)
	}
	if _, err := w.Resume(t.Context(), nil, "run-2"); !errors.Is(err, ErrClosed) {
		t.Fatalf("退出后 Resume err=%v", err)
	}
}

// Activity 覆盖所有工作区的等待中票据；已经送出决定的票不会被 Shutdown 再送一次。
func TestActivityAndShutdownSkipDecided(t *testing.T) {
	w := NewWaiter()
	a := waitTestTicket("ap-a", "run-a")
	b := waitTestTicket("ap-b", "run-b")
	b.WorkspaceID = "云杉"
	results := make(chan approval.Decision, 2)
	for _, tk := range []approval.Ticket{a, b} {
		go func() {
			d, _ := w.Request(t.Context(), tk)
			results <- d
		}()
	}
	waitLive(t, w, "run-a")
	waitLive(t, w, "run-b")
	act := w.Activity()
	if len(act.Waiting) != 2 {
		t.Fatalf("activity=%+v", act)
	}
	if err := w.Deliver("ap-a", approval.Decision{Action: approval.ActionApprove}); err != nil {
		t.Fatal(err)
	}
	deferred := w.Shutdown()
	if len(deferred) != 1 || deferred[0].ID != "ap-b" || deferred[0].WorkspaceID != "云杉" {
		t.Fatalf("deferred=%+v", deferred)
	}
	got := map[approval.Action]int{}
	for i := 0; i < 2; i++ {
		got[(<-results).Action]++
	}
	if got[approval.ActionApprove] != 1 || got[approval.ActionDefer] != 1 {
		t.Fatalf("results=%v", got)
	}
}
