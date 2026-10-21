package events

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Recorder 是把事件记在内存里的 Publisher，给测试和还不想接通道的调用方使用。
//
// 它不启动 goroutine。Publish 返回 nil 时，Snapshot 一定能看到这条事件。
// Fail 非 nil 时，校验通过后直接返回该错误，不记录。
// Close 之后 Publish 返回 ErrClosed，已记录的事件仍可 Snapshot。
type Recorder struct {
	mu     sync.Mutex
	events []Event
	next   uint64
	closed bool
	Fail   error
}

var _ Publisher = (*Recorder)(nil)

// Publish 实现 Publisher。序号从 1 开始，只加在真正记下的事件上。
func (r *Recorder) Publish(ctx context.Context, ev Event) error {
	if ctx == nil {
		return errors.New("events: ctx 不能为 nil")
	}
	ev, err := prepare(ev, time.Now())
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	if r.Fail != nil {
		return r.Fail
	}
	r.next++
	ev.Seq = r.next
	r.events = append(r.events, ev)
	return nil
}

// Close 拒绝后续事件。重复调用是安全的。
func (r *Recorder) Close() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
}

// Snapshot 返回已经记录的事件副本。载荷指针仍与内部共享，调用方不要修改。
func (r *Recorder) Snapshot() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Event, len(r.events))
	copy(out, r.events)
	return out
}
