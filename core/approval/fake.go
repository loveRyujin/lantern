package approval

import (
	"context"
	"errors"
	"sync"
)

// Response 是 Fake 依次交回的一个预设。
//
// Err 非 nil 时直接返回该错误，不再看 Action。
// Wait 为 true 时一直阻塞到 ctx 结束，用来表示「不响应」。
// Params 非 nil 且 Action 为批准时，表示篡改参数。
type Response struct {
	Action Action
	Params []byte
	Wait   bool
	Err    error
}

// Fake 是按预设回答的 Approver。每次 Request 消耗 Responses 的第一条。
// 用尽后返回 ErrNoScript。它不执行工具，也不把暂缓变成错误。
type Fake struct {
	mu        sync.Mutex
	Responses []Response
	tickets   []Ticket
}

var _ Approver = (*Fake)(nil)

// Request 实现 Approver。确认单不合法时不消耗预设，也不记入 Tickets。
func (f *Fake) Request(ctx context.Context, ticket Ticket) (Decision, error) {
	if ctx == nil {
		return Decision{}, errors.New("approval: ctx 不能为 nil")
	}
	if err := ticket.Validate(); err != nil {
		return Decision{}, err
	}
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	f.mu.Lock()
	f.tickets = append(f.tickets, cloneTicket(ticket))
	if len(f.Responses) == 0 {
		f.mu.Unlock()
		return Decision{}, ErrNoScript
	}
	resp := f.Responses[0]
	f.Responses = f.Responses[1:]
	f.mu.Unlock()
	if resp.Err != nil {
		return Decision{}, resp.Err
	}
	if resp.Wait {
		<-ctx.Done()
		return Decision{}, ctx.Err()
	}
	decision := Decision{Action: resp.Action, Params: append([]byte(nil), resp.Params...)}
	if err := decision.Validate(); err != nil {
		return Decision{}, err
	}
	return decision, nil
}

// Tickets 返回已经接受的确认单副本。
func (f *Fake) Tickets() []Ticket {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Ticket, len(f.tickets))
	for i, ticket := range f.tickets {
		out[i] = cloneTicket(ticket)
	}
	return out
}

func cloneTicket(ticket Ticket) Ticket {
	ticket.Params = append([]byte(nil), ticket.Params...)
	ticket.Sensitive = append([]string(nil), ticket.Sensitive...)
	return ticket
}
