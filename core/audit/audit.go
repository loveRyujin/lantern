package audit

import (
	"context"
	"sync"
	"time"
)

// Record 是一条审计。Detail 不放参数原文。
type Record struct {
	At     time.Time
	Actor  string
	Action string
	RunID  string
	Detail string
}

// Log 追加一条审计。
type Log interface {
	Append(ctx context.Context, rec Record) error
}

// Memory 把审计记在内存里，测试和还没接库的调用方用。
type Memory struct {
	mu   sync.Mutex
	recs []Record
}

// Append 实现 Log。
func (m *Memory) Append(ctx context.Context, rec Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if rec.At.IsZero() {
		rec.At = time.Now()
	}
	m.mu.Lock()
	m.recs = append(m.recs, rec)
	m.mu.Unlock()
	return nil
}

// Records 返回已经写下的审计副本。
func (m *Memory) Records() []Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Record, len(m.recs))
	copy(out, m.recs)
	return out
}
