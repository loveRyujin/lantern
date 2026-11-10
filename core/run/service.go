package run

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/loveRyujin/lantern/core/approval"
	"github.com/loveRyujin/lantern/core/tools"
)

// Executor 在哈希核对和确认单消费都成功之后执行一次冻结参数。
// Recover 不会调用它。
type Executor func(ctx context.Context, call tools.Call) (tools.Result, error)

// Event 是按 run_id 查到的一条审计。Detail 没有参数原文。
type Event struct {
	At     string
	Actor  string
	Action string
	Detail string
	RunID  string
}

// Suspended 是重启后仍可交给 Approver 的一趟运行。
type Suspended struct {
	RunID  string
	Ticket approval.Ticket
}

// Report 是一次扫描的归类。切片按 run_id 排序。
type Report struct {
	Suspended   []Suspended
	Interrupted []string
	Illegal     []string
	Breakpoints []string
}

// ResumeResult 是 Resume 之后的运行状态。Executed 为 true 表示执行器被调用了一次。
type ResumeResult struct {
	State    string
	Executed bool
}

// Service 是重启恢复入口。审批决定、过期和消费仍走 approval.Machine。
type Service struct {
	db       *sql.DB
	machine  *approval.Machine
	approver approval.Approver
	execute  Executor
	now      func() time.Time
}

// New 使用已经打开、并且跑过 runs 迁移的库。
func New(db *sql.DB) *Service {
	s := &Service{db: db, now: time.Now}
	if db != nil {
		s.machine = approval.New(db)
	}
	return s
}

// SetClock 同时改恢复逻辑和审批状态机的时钟。
func (s *Service) SetClock(now func() time.Time) {
	if s == nil || now == nil {
		return
	}
	s.now = now
	if s.machine != nil {
		s.machine.SetClock(now)
	}
}

// SetApprover 在 Resume 重新征求挂起确认时使用。
func (s *Service) SetApprover(a approval.Approver) {
	if s == nil {
		return
	}
	s.approver = a
}

// SetExecutor 设置恢复后的单次执行器。扫描不会调用它。
func (s *Service) SetExecutor(fn Executor) {
	if s == nil {
		return
	}
	s.execute = fn
}

// Set 记下运行状态。运行行必须已经存在。Agent 循环用它把 awaiting_approval 落库。
func (s *Service) Set(ctx context.Context, runID, status string) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	if runID == "" || !knownState(status) {
		return fmt.Errorf("run: 运行标识或状态不合法")
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE runs SET state = ?, version = version + 1, updated_at = ? WHERE id = ?`,
		status, s.stamp(), runID,
	)
	if err != nil {
		return fmt.Errorf("run: 更新状态: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("run: 更新状态: %w", err)
	}
	if n != 1 {
		return ErrNotFound
	}
	return nil
}

// Begin 在工具执行前把步骤落成 executing。
// 同一步骤号已经存在时返回 ErrStepExists，调用方不得再执行。
func (s *Service) Begin(ctx context.Context, id, runID, toolName string, input []byte) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	if id == "" || runID == "" {
		return fmt.Errorf("run: 步骤标识为空")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var existing string
	err = tx.QueryRowContext(ctx, `SELECT status FROM run_steps WHERE id = ?`, id).Scan(&existing)
	if err == nil {
		return &stepExistsError{Status: existing}
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("run: 读取步骤: %w", err)
	}
	var ordinal int
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(ordinal), -1) + 1 FROM run_steps WHERE run_id = ?`, runID).Scan(&ordinal); err != nil {
		return fmt.Errorf("run: 分配步骤序号: %w", err)
	}
	stamp := s.stamp()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO run_steps (
			id, run_id, ordinal, status, tool_name, input_json, output_json, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, '', ?, ?)`,
		id, runID, ordinal, StepExecuting, toolName, string(input), stamp, stamp,
	)
	if err != nil {
		return fmt.Errorf("run: 写入步骤: %w", err)
	}
	return tx.Commit()
}

// Finish 把 executing 的步骤收成 done 或 error。
func (s *Service) Finish(ctx context.Context, id, status string) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	if id == "" || (status != StepDone && status != StepError) {
		return fmt.Errorf("run: 步骤结束状态不合法")
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE run_steps SET status = ?, updated_at = ? WHERE id = ? AND status = ?`,
		status, s.stamp(), id, StepExecuting,
	)
	if err != nil {
		return fmt.Errorf("run: 结束步骤: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("run: 结束步骤: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("run: 步骤 %s 不是 executing", id)
	}
	return nil
}

// Events 返回这个 run_id 上的审计，按写入顺序。
func (s *Service) Events(ctx context.Context, runID string) ([]Event, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	if runID == "" {
		return nil, fmt.Errorf("run: 运行标识为空")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT at, actor, action, detail, run_id
		FROM audit
		WHERE run_id = ?
		ORDER BY id`, runID)
	if err != nil {
		return nil, fmt.Errorf("run: 查询审计: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var ev Event
		if err := rows.Scan(&ev.At, &ev.Actor, &ev.Action, &ev.Detail, &ev.RunID); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Service) ready(ctx context.Context) error {
	if ctx == nil {
		return errors.New("run: ctx 不能为 nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.db == nil {
		return ErrNoDB
	}
	return nil
}

func (s *Service) stamp() string {
	return s.clock().Format(time.RFC3339Nano)
}

func (s *Service) clock() time.Time {
	if s != nil && s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

type runRow struct {
	id      string
	state   string
	version int64
}

func loadRun(ctx context.Context, q rowQuery, id string) (runRow, error) {
	var rec runRow
	err := q.QueryRowContext(ctx, `SELECT id, state, version FROM runs WHERE id = ?`, id).Scan(&rec.id, &rec.state, &rec.version)
	if errors.Is(err, sql.ErrNoRows) {
		return runRow{}, ErrNotFound
	}
	if err != nil {
		return runRow{}, fmt.Errorf("run: 读取运行: %w", err)
	}
	return rec, nil
}

func lastStep(ctx context.Context, q rowQuery, runID string) (id, status string, err error) {
	err = q.QueryRowContext(ctx, `
		SELECT id, status FROM run_steps
		WHERE run_id = ?
		ORDER BY ordinal DESC
		LIMIT 1`, runID).Scan(&id, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("run: 读取步骤: %w", err)
	}
	return id, status, nil
}

type rowQuery interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func casState(ctx context.Context, q rowQuery, id, from, to string, version int64, at string) error {
	res, err := q.ExecContext(ctx, `
		UPDATE runs
		SET state = ?, version = version + 1, updated_at = ?
		WHERE id = ? AND state = ? AND version = ?`,
		to, at, id, from, version,
	)
	if err != nil {
		return fmt.Errorf("run: 更新状态: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("run: 更新状态: %w", err)
	}
	if n != 1 {
		return ErrConflict
	}
	return nil
}

func insertAudit(ctx context.Context, q rowQuery, at time.Time, action, runID, detail string) error {
	_, err := q.ExecContext(ctx,
		`INSERT INTO audit (at, actor, action, detail, run_id) VALUES (?, 'run', ?, ?, ?)`,
		at.UTC().Format(time.RFC3339Nano), action, detail, runID,
	)
	if err != nil {
		return fmt.Errorf("run: 写审计: %w", err)
	}
	return nil
}

func auditDetail(ticketID, stepID, policy string) string {
	var b strings.Builder
	if ticketID != "" {
		b.WriteString("ticket=")
		b.WriteString(ticketID)
		b.WriteByte(' ')
	}
	b.WriteString("step=")
	b.WriteString(stepID)
	b.WriteString(" policy=")
	b.WriteString(policy)
	return b.String()
}

const (
	kindNone kind = iota
	kindPending
	kindApproved
)

type kind int

func loadRestorable(ctx context.Context, q rowQuery, runID string) (approval.Ticket, kind, error) {
	var (
		id, stepID, hash, params, workspaceID, action, object, expires, state, risk string
	)
	err := q.QueryRowContext(ctx, `
		SELECT id, step_id, params_hash, params_json, workspace_id, action, export_object, expires_at, state, risk
		FROM approvals
		WHERE run_id = ?
		  AND state IN ('pending', 'approved')
		  AND (consumed_at IS NULL OR consumed_at = '')
		ORDER BY CASE state WHEN 'pending' THEN 0 ELSE 1 END, created_at DESC, id DESC
		LIMIT 1`, runID).Scan(&id, &stepID, &hash, &params, &workspaceID, &action, &object, &expires, &state, &risk)
	if errors.Is(err, sql.ErrNoRows) {
		return approval.Ticket{}, kindNone, nil
	}
	if err != nil {
		return approval.Ticket{}, kindNone, fmt.Errorf("run: 读取确认单: %w", err)
	}
	ticket := approval.Ticket{
		ID:           id,
		RunID:        runID,
		StepID:       stepID,
		WorkspaceID:  workspaceID,
		Action:       action,
		Params:       append([]byte(nil), params...),
		ParamsHash:   hash,
		ExportObject: object,
		Risk:         risk,
	}
	if exp, ok := parseTime(expires); ok {
		ticket.ExpiresAt = exp
	}
	if state == string(approval.StatePending) {
		return ticket, kindPending, nil
	}
	return ticket, kindApproved, nil
}

func parseTime(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	exp, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		exp, err = time.Parse(time.RFC3339, value)
		if err != nil {
			return time.Time{}, false
		}
	}
	return exp, true
}
