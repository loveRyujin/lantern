package events_test

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/events"
)

func TestStreamOrderAndClose(t *testing.T) {
	stream := events.NewStream(4)
	t.Cleanup(stream.Close)
	ctx := context.Background()
	when := time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)
	inputs := []events.Event{
		{Kind: events.KindToken, RunID: "run", Time: when, Seq: 99, Token: &events.Token{Text: "你"}},
		{Kind: events.KindStep, RunID: "run", Step: &events.Step{ID: "s1", Status: events.StepExecuting, Summary: "sql"}},
		{Kind: events.KindApprovalRequest, RunID: "run", Approval: &events.ApprovalRequest{TicketID: "ap1", Action: "export_data"}},
		{Kind: events.KindDone, RunID: "run", Done: &events.Done{Status: events.DoneCompleted, Text: "完成"}},
	}
	for _, ev := range inputs {
		if err := stream.Publish(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	for i := range inputs {
		got := readEvent(t, stream)
		if got.Seq != uint64(i+1) || got.Kind != inputs[i].Kind || got.RunID != "run" || got.Time.IsZero() {
			t.Fatalf("事件 %d = %+v", i, got)
		}
		if i == 0 && (got.Time != when || got.Token == nil || got.Token.Text != "你") {
			t.Fatalf("调用方指定的时间和文本应该保留: %+v", got)
		}
	}
}

func TestStreamPreservesOrder(t *testing.T) {
	stream := events.NewStream(8)
	t.Cleanup(stream.Close)
	ctx := context.Background()
	for _, text := range []string{"a", "b", "c"} {
		if err := stream.Publish(ctx, token("run", text)); err != nil {
			t.Fatal(err)
		}
	}
	for i, text := range []string{"a", "b", "c"} {
		got := readEvent(t, stream)
		if got.Token.Text != text || got.Seq != uint64(i+1) {
			t.Fatalf("got %+v", got)
		}
	}
}

func TestStreamBackpressureAndClose(t *testing.T) {
	stream := events.NewStream(0)
	defer stream.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := stream.Publish(ctx, token("run", "blocked"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("背压 err=%v", err)
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Fatal("缓冲为 0 且没有读者时，Publish 应该等待")
	}

	unblocked := events.NewStream(0)
	errCh := make(chan error, 1)
	go func() {
		errCh <- unblocked.Publish(context.Background(), token("run", "x"))
	}()
	unblocked.Close()
	select {
	case err := <-errCh:
		if !errors.Is(err, events.ErrClosed) {
			t.Fatalf("Close 之后 err=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close 没有解开 Publish")
	}
	if err := unblocked.Publish(context.Background(), token("run", "y")); !errors.Is(err, events.ErrClosed) {
		t.Fatalf("关闭后再次 Publish err=%v", err)
	}
}

func TestStreamConcurrentPublish(t *testing.T) {
	stream := events.NewStream(4)
	var wg sync.WaitGroup
	const n = 20
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errCh <- stream.Publish(context.Background(), token("run", strconv.Itoa(i)))
		}(i)
	}
	got := make([]events.Event, 0, n)
	done := make(chan struct{})
	go func() {
		for ev := range stream.Events() {
			got = append(got, ev)
			if len(got) == n {
				close(done)
				return
			}
		}
		close(done)
	}()
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("没有收齐并发事件")
	}
	stream.Close()
	seen := map[string]bool{}
	var prev uint64
	for _, ev := range got {
		if ev.Seq <= prev {
			t.Fatalf("序号没有递增: %d 然后 %d", prev, ev.Seq)
		}
		prev = ev.Seq
		seen[ev.Token.Text] = true
	}
	if len(seen) != n || prev != n {
		t.Fatalf("收到 %d 条，最后序号 %d", len(seen), prev)
	}
}

func TestPublishRejectsInvalidEvent(t *testing.T) {
	stream := events.NewStream(1)
	t.Cleanup(stream.Close)
	bad := []events.Event{
		{},
		{Kind: events.KindToken, RunID: "run"},
		{Kind: events.KindToken, RunID: "run", Token: &events.Token{Text: "x"}, Step: &events.Step{ID: "s", Status: events.StepDone}},
		{Kind: events.KindStep, RunID: "run", Step: &events.Step{ID: "s", Status: "nope"}},
		{Kind: events.KindDone, RunID: "run", Done: &events.Done{Status: "running"}},
		{Kind: "nope", RunID: "run", Token: &events.Token{Text: "x"}},
	}
	for _, ev := range bad {
		if err := stream.Publish(context.Background(), ev); err == nil {
			t.Fatalf("非法事件被接受: %+v", ev)
		}
	}
	if err := stream.Publish(context.Background(), token("run", "ok")); err != nil {
		t.Fatal(err)
	}
	got := readEvent(t, stream)
	if got.Seq != 1 || got.Token.Text != "ok" {
		t.Fatalf("非法事件占用了序号: %+v", got)
	}
}

func TestRecorder(t *testing.T) {
	rec := &events.Recorder{}
	var pub events.Publisher = rec
	ctx := context.Background()
	if err := pub.Publish(ctx, token("run", "a")); err != nil {
		t.Fatal(err)
	}
	if err := pub.Publish(ctx, events.Event{Kind: events.KindDone, RunID: "run", Done: &events.Done{Status: events.DoneFailed}}); err != nil {
		t.Fatal(err)
	}
	snap := rec.Snapshot()
	if len(snap) != 2 || snap[0].Seq != 1 || snap[1].Seq != 2 || snap[1].Done.Status != events.DoneFailed {
		t.Fatalf("snapshot = %+v", snap)
	}
	rec.Fail = errors.New("注入失败")
	if err := pub.Publish(ctx, token("run", "b")); err == nil {
		t.Fatal("Fail 没有生效")
	}
	if len(rec.Snapshot()) != 2 {
		t.Fatal("失败的 Publish 被记下来了")
	}
	rec.Fail = nil
	rec.Close()
	if err := pub.Publish(ctx, token("run", "c")); !errors.Is(err, events.ErrClosed) {
		t.Fatalf("Close 后 err=%v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	rec2 := &events.Recorder{}
	if err := rec2.Publish(cancelled, token("run", "z")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestNilContext(t *testing.T) {
	stream := events.NewStream(1)
	t.Cleanup(stream.Close)
	if err := stream.Publish(nil, token("run", "a")); err == nil {
		t.Fatal("Stream 接受了 nil ctx")
	}
	if err := (&events.Recorder{}).Publish(nil, token("run", "a")); err == nil {
		t.Fatal("Recorder 接受了 nil ctx")
	}
}

func token(runID, text string) events.Event {
	return events.Event{Kind: events.KindToken, RunID: runID, Token: &events.Token{Text: text}}
}

func readEvent(t *testing.T, stream *events.Stream) events.Event {
	t.Helper()
	select {
	case ev, ok := <-stream.Events():
		if !ok {
			t.Fatal("事件流已关闭")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("等待事件超时")
		return events.Event{}
	}
}
