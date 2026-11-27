package session

import (
	"context"
	"errors"
	"sync"

	"github.com/loveRyujin/lantern/core/approval"
	"github.com/loveRyujin/lantern/core/run"
)

var (
	// ErrBusy 表示这趟运行的确认还在等决定，或者已经有一次 Resume 在进行。
	ErrBusy = errors.New("session: 确认正在等待")
	// ErrDecided 表示这张单的决定已经送出。
	ErrDecided = errors.New("session: 决定已经送出")
	// ErrNoWait 表示当前没有在等这张确认单。
	ErrNoWait = errors.New("session: 没有等待中的确认单")
	// ErrParamsLocked 表示界面试图替换冻结参数。
	ErrParamsLocked = errors.New("session: 界面不能修改冻结参数")
	// ErrClosed 表示应用正在退出，不再开始新的 Resume。
	ErrClosed = errors.New("session: 正在退出")
)

// Waiter 实现 Approver。它只把决定交回调用方，不写状态机，也不执行工具。
// 同一 run 上，Request 未返回时 Resume 直接拒绝。
type Waiter struct {
	mu       sync.Mutex
	live     map[string]*hold
	byTicket map[string]*hold
	resuming map[string]struct{}
	onShow   func(approval.Ticket)
	closed   bool
}

type hold struct {
	ticket approval.Ticket
	ch     chan approval.Decision
	sent   bool
}

// NewWaiter 构造一个空等待器。
func NewWaiter() *Waiter {
	return &Waiter{
		live:     map[string]*hold{},
		byTicket: map[string]*hold{},
		resuming: map[string]struct{}{},
	}
}

// SetOnShow 在开始等待前同步调用。用来把票据交给界面。
func (w *Waiter) SetOnShow(fn func(approval.Ticket)) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.onShow = fn
	w.mu.Unlock()
}

// Request 实现 approval.Approver。决定里只要带了参数就拒绝，不交给循环执行。
func (w *Waiter) Request(ctx context.Context, ticket approval.Ticket) (approval.Decision, error) {
	if ctx == nil {
		return approval.Decision{}, errors.New("session: ctx 不能为 nil")
	}
	if err := ticket.Validate(); err != nil {
		return approval.Decision{}, err
	}
	h := &hold{ticket: cloneTicket(ticket), ch: make(chan approval.Decision, 1)}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return approval.Decision{Action: approval.ActionDefer}, nil
	}
	if _, exists := w.live[ticket.RunID]; exists {
		w.mu.Unlock()
		return approval.Decision{}, ErrBusy
	}
	w.live[ticket.RunID] = h
	w.byTicket[ticket.ID] = h
	show := w.onShow
	w.mu.Unlock()
	if show != nil {
		show(h.ticket)
	}
	defer w.clear(ticket.RunID, ticket.ID, h)

	var d approval.Decision
	select {
	case d = <-h.ch:
	case <-ctx.Done():
		// 退出时先送暂缓再取消 ctx。两者同时就绪时 select 随机挑，这里补收一次，暂缓不会丢。
		select {
		case d = <-h.ch:
		default:
			return approval.Decision{}, ctx.Err()
		}
	}
	if err := d.Validate(); err != nil {
		return approval.Decision{}, err
	}
	if len(d.Params) > 0 {
		return approval.Decision{}, ErrParamsLocked
	}
	return d, nil
}

// Deliver 把一次决定交给正在等待的确认单。第二次送出会失败，通道里仍只有第一次。
func (w *Waiter) Deliver(ticketID string, d approval.Decision) error {
	if w == nil {
		return ErrNoWait
	}
	if err := d.Validate(); err != nil {
		return err
	}
	if len(d.Params) > 0 {
		return ErrParamsLocked
	}
	w.mu.Lock()
	h := w.byTicket[ticketID]
	if h == nil {
		w.mu.Unlock()
		return ErrNoWait
	}
	if h.sent {
		w.mu.Unlock()
		return ErrDecided
	}
	h.sent = true
	h.ch <- d
	w.mu.Unlock()
	return nil
}

// Shutdown 在退出时调用。正在等待的确认单都收到暂缓，之后到达的 Request 也直接暂缓。
// 返回收到暂缓的票据，覆盖所有工作区。调用方随后才能取消 ctx。
func (w *Waiter) Shutdown() []approval.Ticket {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	var out []approval.Ticket
	for _, h := range w.live {
		if h.sent {
			continue
		}
		h.sent = true
		h.ch <- approval.Decision{Action: approval.ActionDefer}
		out = append(out, cloneTicket(h.ticket))
	}
	return out
}

// Activity 是等待器里本进程的活跃工作：等决定的确认单（不分工作区）和进行中的 Resume。
type Activity struct {
	Waiting  []approval.Ticket
	Resuming []string
}

// Activity 返回当前快照。已暂缓、库里仍是 awaiting_approval 但没人在等的运行不在其中。
func (w *Waiter) Activity() Activity {
	if w == nil {
		return Activity{}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var a Activity
	for _, h := range w.live {
		a.Waiting = append(a.Waiting, cloneTicket(h.ticket))
	}
	for id := range w.resuming {
		a.Resuming = append(a.Resuming, id)
	}
	return a
}

// Busy 报告这趟运行是否还卡在确认或 Resume 里。
func (w *Waiter) Busy(runID string) bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.live[runID]; ok {
		return true
	}
	_, ok := w.resuming[runID]
	return ok
}

// Current 返回正在等待的票据副本。
func (w *Waiter) Current(runID string) (approval.Ticket, bool) {
	if w == nil {
		return approval.Ticket{}, false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	h := w.live[runID]
	if h == nil {
		return approval.Ticket{}, false
	}
	return cloneTicket(h.ticket), true
}

// Resume 在没有活跃等待时调用 run.Service.Resume。
// 活跃 Loop 还堵在 Request 时返回 ErrBusy，并且不会调用 svc.Resume。
func (w *Waiter) Resume(ctx context.Context, svc *run.Service, runID string) (run.ResumeResult, error) {
	if w == nil {
		return run.ResumeResult{}, ErrNoWait
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return run.ResumeResult{}, ErrClosed
	}
	if _, ok := w.live[runID]; ok {
		w.mu.Unlock()
		return run.ResumeResult{}, ErrBusy
	}
	if _, ok := w.resuming[runID]; ok {
		w.mu.Unlock()
		return run.ResumeResult{}, ErrBusy
	}
	w.resuming[runID] = struct{}{}
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		delete(w.resuming, runID)
		w.mu.Unlock()
	}()
	if svc == nil {
		return run.ResumeResult{}, run.ErrNoDB
	}
	return svc.Resume(ctx, runID)
}

func (w *Waiter) clear(runID, ticketID string, h *hold) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.live[runID] == h {
		delete(w.live, runID)
	}
	if w.byTicket[ticketID] == h {
		delete(w.byTicket, ticketID)
	}
}

func cloneTicket(ticket approval.Ticket) approval.Ticket {
	ticket.Params = append([]byte(nil), ticket.Params...)
	ticket.Sensitive = append([]string(nil), ticket.Sensitive...)
	return ticket
}
