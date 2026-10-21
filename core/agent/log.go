package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"
	"unicode/utf8"
)

const maxSummaryRunes = 200

// RunEvent 是写入 run_events 的一条工具步骤。没有参数原文。
type RunEvent struct {
	RunID      string
	Kind       string
	ToolName   string
	ParamsHash string
	Summary    string
	Duration   time.Duration
	Tokens     int
}

type eventPayload struct {
	ToolName   string `json:"tool_name"`
	ParamsHash string `json:"params_hash"`
	Summary    string `json:"summary"`
	DurationNS int64  `json:"duration_ns"`
	Tokens     int    `json:"tokens"`
}

// RunLog 追加 run_events。
type RunLog interface {
	Append(ctx context.Context, ev RunEvent) error
}

// MemoryLog 把 run_events 记在内存里。
type MemoryLog struct {
	mu     sync.Mutex
	events []RunEvent
}

// Append 实现 RunLog。
func (m *MemoryLog) Append(ctx context.Context, ev RunEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	m.events = append(m.events, ev)
	m.mu.Unlock()
	return nil
}

// Events 返回副本。
func (m *MemoryLog) Events() []RunEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]RunEvent, len(m.events))
	copy(out, m.events)
	return out
}

// SQLiteLog 把 run_events 写入 runs.db。payload 只有哈希和摘要，没有参数原文。
type SQLiteLog struct {
	db *sql.DB
}

// NewSQLiteLog 使用已经打开的 runs 库。调用方需要先有对应的 runs 行。
func NewSQLiteLog(db *sql.DB) *SQLiteLog {
	return &SQLiteLog{db: db}
}

// Append 插入一行。
func (s *SQLiteLog) Append(ctx context.Context, ev RunEvent) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("agent: 没有 run_events 库")
	}
	payload, err := json.Marshal(eventPayload{
		ToolName:   ev.ToolName,
		ParamsHash: ev.ParamsHash,
		Summary:    ev.Summary,
		DurationNS: ev.Duration.Nanoseconds(),
		Tokens:     ev.Tokens,
	})
	if err != nil {
		return err
	}
	kind := ev.Kind
	if kind == "" {
		kind = "step"
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO run_events (run_id, kind, payload, created_at) VALUES (?, ?, ?, ?)`,
		ev.RunID, kind, string(payload), time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("agent: 写 run_events: %w", err)
	}
	return nil
}

func clipSummary(s string) string {
	if s == "" {
		return "执行完成"
	}
	if utf8.RuneCountInString(s) <= maxSummaryRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxSummaryRunes])
}

func clipText(s string) string {
	const max = 500
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
