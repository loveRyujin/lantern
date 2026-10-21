package approval

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	defaultTTL = 24 * time.Hour
	timeLayout = time.RFC3339Nano
)

// transitionSQL 是唯一的状态更新。条件里同时卡住 state 和 version，
// 影响行数必须是 1，避免并发下两次消费都成功。
const transitionSQL = `
UPDATE approvals
SET state = ?, version = version + 1, decision = ?, decided_at = ?, consumed_at = ?
WHERE id = ? AND state = ? AND version = ?`

const insertSQL = `
INSERT INTO approvals (
	id, run_id, step_id, params_hash, params_json, decision, created_at,
	workspace_id, action, export_object, expires_at, state, version
) VALUES (?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?, 0)`

// Machine 是审批状态机。确认单的插入、决定、过期和消费都从这里写。
type Machine struct {
	db  *sql.DB
	now func() time.Time
	// gate 在读完确认单、写之前调用。测试用它把两个事务停在同一版本上。
	gate func()
}

var (
	_ Store    = (*Machine)(nil)
	_ Decider  = (*Machine)(nil)
	_ Consumer = (*Machine)(nil)
)

// New 使用已经打开的 runs 库。
func New(db *sql.DB) *Machine {
	return &Machine{db: db, now: time.Now}
}

// SetClock 换成注入的时钟。测试用它卡在 expires_at 上。
func (m *Machine) SetClock(now func() time.Time) {
	if m == nil || now == nil {
		return
	}
	m.now = now
}

// SetGate 在读完行、写库之前调用。并发测试用它卡住同一版本。
func (m *Machine) SetGate(fn func()) {
	if m == nil {
		return
	}
	m.gate = fn
}

func (m *Machine) hitGate() {
	if m != nil && m.gate != nil {
		m.gate()
	}
}

func (m *Machine) clock() time.Time {
	if m != nil && m.now != nil {
		return m.now().UTC()
	}
	return time.Now().UTC()
}

// Transact 开一个事务把种子运行和确认单写在一起。失败就整段回滚。
// 状态机自己提交的审计错误会保留下来。
func (m *Machine) Transact(ctx context.Context, fn func(*sql.Tx) error) error {
	return m.transact(ctx, fn)
}

// Create 插入一张 pending 确认单。版本从 0 开始。
func (m *Machine) Create(ctx context.Context, ticket Ticket) error {
	return m.transact(ctx, func(tx *sql.Tx) error {
		return m.CreateTx(ctx, tx, ticket)
	})
}

// CreateTx 在调用方的事务里插入 pending 确认单。
func (m *Machine) CreateTx(ctx context.Context, tx *sql.Tx, ticket Ticket) error {
	if tx == nil {
		return errors.New("approval: 没有事务")
	}
	return m.insert(ctx, tx, ticket)
}

// Decide 按 Approver 的决定转移。暂缓保持 pending，不改版本。
// 先读出已提交的行，测试用的闸门也停在写锁之外，两个事务才能拿着同一版本去做条件更新。
func (m *Machine) Decide(ctx context.Context, id string, decision Decision) error {
	if err := decision.Validate(); err != nil {
		return err
	}
	rec, err := m.loadCommitted(ctx, id)
	if err != nil {
		return err
	}
	m.hitGate()
	return m.transact(ctx, func(tx *sql.Tx) error {
		return m.finishDecide(ctx, tx, rec, decision)
	})
}

// DecideTx 在调用方的事务里写决定。
func (m *Machine) DecideTx(ctx context.Context, tx *sql.Tx, id string, decision Decision) error {
	if tx == nil {
		return errors.New("approval: 没有事务")
	}
	return m.decide(ctx, tx, id, decision)
}

// Expire 把已经到期限、还没消费的 pending 或 approved 转成 expired。
// 已消费的确认单保持 consumed。
func (m *Machine) Expire(ctx context.Context, id string) error {
	rec, err := m.loadCommitted(ctx, id)
	if err != nil {
		return err
	}
	m.hitGate()
	return m.transact(ctx, func(tx *sql.Tx) error {
		return m.finishExpire(ctx, tx, rec)
	})
}

// Consume 核对调用方带来的运行、步骤和参数哈希，再把 approved 消费掉。
func (m *Machine) Consume(ctx context.Context, id string, cc ConsumeContext) error {
	return m.consumeCommitted(ctx, id, cc, exportScope{})
}

// ConsumeForExport 在消费前再核对工作区、动作和导出对象。
func (m *Machine) ConsumeForExport(ctx context.Context, id string, cc ConsumeContext, workspaceID, object string) error {
	return m.consumeCommitted(ctx, id, cc, exportScope{
		check:       true,
		workspaceID: workspaceID,
		object:      object,
	})
}

func (m *Machine) loadCommitted(ctx context.Context, id string) (row, error) {
	if err := ctx.Err(); err != nil {
		return row{}, err
	}
	if m == nil || m.db == nil {
		return row{}, errors.New("approval: 没有数据库")
	}
	return load(ctx, m.db, id)
}

func (m *Machine) consumeCommitted(ctx context.Context, id string, cc ConsumeContext, scope exportScope) error {
	rec, err := m.loadCommitted(ctx, id)
	if err != nil {
		return err
	}
	if rec.runID != cc.RunID || rec.stepID != cc.StepID || rec.paramsHash != cc.ArgsHash {
		return ErrMismatch
	}
	m.hitGate()
	return m.transact(ctx, func(tx *sql.Tx) error {
		return m.finishConsume(ctx, tx, rec, scope)
	})
}

type exportScope struct {
	check       bool
	workspaceID string
	object      string
}

type row struct {
	id           string
	runID        string
	stepID       string
	paramsHash   string
	paramsJSON   string
	decision     string
	decidedAt    sql.NullString
	workspaceID  string
	action       string
	exportObject string
	expiresAt    string
	consumedAt   sql.NullString
	state        State
	version      int64
}

type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func (m *Machine) transact(ctx context.Context, fn func(*sql.Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m == nil || m.db == nil {
		return errors.New("approval: 没有数据库")
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	err = fn(tx)
	var committed commitError
	if errors.As(err, &committed) {
		if cerr := tx.Commit(); cerr != nil {
			return cerr
		}
		return committed.err
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// commitError 表示事务里已经写了要保留的内容（审计，或一次成功的过期转移），
// 但调用方仍然要看到这个错误。
type commitError struct{ err error }

func (e commitError) Error() string { return e.err.Error() }

func (e commitError) Unwrap() error { return e.err }

func (m *Machine) insert(ctx context.Context, q queryRower, ticket Ticket) error {
	if err := ticket.Validate(); err != nil {
		return err
	}
	if ticket.StepID == "" {
		return fmt.Errorf("%w: 缺少步骤", ErrInvalidTicket)
	}
	ok, err := SnapshotMatches(ticket.ParamsHash, ticket.Params)
	if err != nil {
		return err
	}
	if !ok {
		return ErrSnapshotMismatch
	}
	now := m.clock()
	exp := ticket.ExpiresAt
	if exp.IsZero() {
		exp = now.Add(defaultTTL)
	}
	_, err = q.ExecContext(ctx, insertSQL,
		ticket.ID, ticket.RunID, ticket.StepID, ticket.ParamsHash, string(ticket.Params),
		now.Format(timeLayout), ticket.WorkspaceID, ticket.Action, ticket.ExportObject,
		exp.UTC().Format(timeLayout), string(StatePending),
	)
	if err != nil {
		return fmt.Errorf("approval: 写入确认单: %w", err)
	}
	return nil
}

func (m *Machine) decide(ctx context.Context, q queryRower, id string, decision Decision) error {
	if err := decision.Validate(); err != nil {
		return err
	}
	rec, err := load(ctx, q, id)
	if err != nil {
		return err
	}
	m.hitGate()
	return m.finishDecide(ctx, q, rec, decision)
}

func (m *Machine) finishDecide(ctx context.Context, q queryRower, rec row, decision Decision) error {
	if err := consistent(rec); err != nil {
		return m.fail(ctx, q, rec, "corrupt", err)
	}
	if decision.Action == ActionDefer {
		if rec.state != StatePending {
			return m.deny(ctx, q, rec, rec.state)
		}
		return nil
	}
	if decision.OverridesSnapshot() {
		sum, err := ParamsHash(decision.Params)
		if err != nil {
			return err
		}
		if sum != rec.paramsHash {
			return m.fail(ctx, q, rec, "snapshot_mismatch", ErrSnapshotMismatch)
		}
	}
	if m.due(rec) && (rec.state == StatePending || rec.state == StateApproved) {
		if err := m.transition(ctx, q, rec, StateExpired, rec.decision, rec.decidedAt, rec.consumedAt); err != nil {
			return err
		}
		return commitError{err: ErrExpired}
	}
	switch decision.Action {
	case ActionApprove:
		decided := m.clock().Format(timeLayout)
		return m.transition(ctx, q, rec, StateApproved, string(ActionApprove), sql.NullString{String: decided, Valid: true}, rec.consumedAt)
	case ActionReject:
		decided := m.clock().Format(timeLayout)
		return m.transition(ctx, q, rec, StateRejected, string(ActionReject), sql.NullString{String: decided, Valid: true}, rec.consumedAt)
	default:
		return fmt.Errorf("%w: 未知决定 %q", ErrInvalidDecision, decision.Action)
	}
}

func (m *Machine) finishExpire(ctx context.Context, q queryRower, rec row) error {
	if err := consistent(rec); err != nil {
		return m.fail(ctx, q, rec, "corrupt", err)
	}
	if rec.state == StateConsumed {
		return m.deny(ctx, q, rec, StateExpired)
	}
	if !m.due(rec) {
		if rec.state != StatePending && rec.state != StateApproved {
			return m.deny(ctx, q, rec, StateExpired)
		}
		return ErrNotDue
	}
	if err := m.transition(ctx, q, rec, StateExpired, rec.decision, rec.decidedAt, rec.consumedAt); err != nil {
		return err
	}
	return nil
}

func (m *Machine) finishConsume(ctx context.Context, q queryRower, rec row, scope exportScope) error {
	if err := consistent(rec); err != nil {
		return m.fail(ctx, q, rec, "corrupt", err)
	}
	if !validHash(rec.paramsHash) || !lowerHex(rec.paramsHash) {
		return m.fail(ctx, q, rec, "snapshot_mismatch", ErrSnapshotMismatch)
	}
	sum, err := ParamsHash([]byte(rec.paramsJSON))
	if err != nil || sum != rec.paramsHash {
		return m.fail(ctx, q, rec, "snapshot_mismatch", ErrSnapshotMismatch)
	}
	if scope.check {
		if rec.workspaceID != scope.workspaceID || rec.action != "export_data" || rec.exportObject != scope.object {
			return ErrScope
		}
	}
	if m.due(rec) && (rec.state == StatePending || rec.state == StateApproved) {
		if err := m.transition(ctx, q, rec, StateExpired, rec.decision, rec.decidedAt, rec.consumedAt); err != nil {
			return err
		}
		return commitError{err: ErrExpired}
	}
	consumed := sql.NullString{String: m.clock().Format(timeLayout), Valid: true}
	return m.transition(ctx, q, rec, StateConsumed, rec.decision, rec.decidedAt, consumed)
}

func (m *Machine) transition(ctx context.Context, q queryRower, rec row, to State, decision string, decidedAt, consumedAt sql.NullString) error {
	if !legal(rec.state, to) {
		return m.deny(ctx, q, rec, to)
	}
	var decided, consumed any
	if decidedAt.Valid {
		decided = decidedAt.String
	}
	if consumedAt.Valid {
		consumed = consumedAt.String
	}
	res, err := q.ExecContext(ctx, transitionSQL,
		string(to), decision, decided, consumed,
		rec.id, string(rec.state), rec.version,
	)
	if err != nil {
		return fmt.Errorf("approval: 更新状态: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("approval: 更新状态: %w", err)
	}
	if n != 1 {
		// 条件里的 state 和 version 已经对不上。在同一个事务里再读，
		// 可能仍是旧快照，所以这里直接当成版本冲突，不再记一次非法转移。
		return ErrVersionConflict
	}
	return nil
}

func (m *Machine) deny(ctx context.Context, q queryRower, rec row, to State) error {
	err := &transitionError{from: rec.state, to: to}
	return m.fail(ctx, q, rec, "illegal_transition", err)
}

func (m *Machine) fail(ctx context.Context, q queryRower, rec row, action string, cause error) error {
	if action == "snapshot_mismatch" {
		// 参数快照对不上就拒绝执行，并把这次运行标成失败。审计里不放参数原文。
		if err := m.markRunFailed(ctx, q, rec.runID); err != nil {
			return err
		}
	}
	detail := fmt.Sprintf("id=%s run=%s from=%s action=%s", rec.id, rec.runID, rec.state, action)
	if err := m.appendAudit(ctx, q, action, detail); err != nil {
		return err
	}
	return commitError{err: cause}
}

func (m *Machine) markRunFailed(ctx context.Context, q queryRower, runID string) error {
	_, err := q.ExecContext(ctx, `
		UPDATE runs SET state = 'failed', version = version + 1, updated_at = ? WHERE id = ?`,
		m.clock().Format(timeLayout), runID,
	)
	if err != nil {
		return fmt.Errorf("approval: 运行标为失败: %w", err)
	}
	return nil
}

func (m *Machine) appendAudit(ctx context.Context, q queryRower, action, detail string) error {
	_, err := q.ExecContext(ctx,
		`INSERT INTO audit (at, actor, action, detail) VALUES (?, 'approval', ?, ?)`,
		m.clock().Format(timeLayout), action, detail,
	)
	if err != nil {
		return fmt.Errorf("approval: 写审计: %w", err)
	}
	return nil
}

func (m *Machine) due(rec row) bool {
	exp, ok := parseTime(rec.expiresAt)
	if !ok {
		return true
	}
	return !exp.After(m.clock())
}

func parseTime(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	exp, err := time.Parse(timeLayout, value)
	if err != nil {
		exp, err = time.Parse(time.RFC3339, value)
		if err != nil {
			return time.Time{}, false
		}
	}
	return exp, true
}

func consistent(rec row) error {
	consumed := rec.consumedAt.Valid && rec.consumedAt.String != ""
	switch rec.state {
	case StatePending:
		if consumed || (rec.decision != "" && rec.decision != string(ActionDefer)) {
			return ErrCorrupt
		}
	case StateApproved:
		if consumed || rec.decision != string(ActionApprove) {
			return ErrCorrupt
		}
	case StateRejected:
		if consumed || rec.decision != string(ActionReject) {
			return ErrCorrupt
		}
	case StateExpired:
		if consumed {
			return ErrCorrupt
		}
		if rec.decision != "" && rec.decision != string(ActionApprove) && rec.decision != string(ActionReject) && rec.decision != string(ActionDefer) {
			return ErrCorrupt
		}
	case StateConsumed:
		if !consumed || rec.decision != string(ActionApprove) {
			return ErrCorrupt
		}
	default:
		return ErrCorrupt
	}
	return nil
}

func load(ctx context.Context, q queryRower, id string) (row, error) {
	var rec row
	var state string
	err := q.QueryRowContext(ctx, `
		SELECT id, run_id, step_id, params_hash, params_json, decision, decided_at,
			workspace_id, action, export_object, expires_at, consumed_at, state, version
		FROM approvals WHERE id = ?`, id,
	).Scan(
		&rec.id, &rec.runID, &rec.stepID, &rec.paramsHash, &rec.paramsJSON, &rec.decision, &rec.decidedAt,
		&rec.workspaceID, &rec.action, &rec.exportObject, &rec.expiresAt, &rec.consumedAt, &state, &rec.version,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return row{}, ErrNotFound
	}
	if err != nil {
		return row{}, fmt.Errorf("approval: 读取确认单: %w", err)
	}
	rec.state = State(state)
	return rec, nil
}

func lowerHex(h string) bool {
	if !validHash(h) {
		return false
	}
	for _, r := range h {
		if r >= 'A' && r <= 'F' {
			return false
		}
	}
	return true
}
