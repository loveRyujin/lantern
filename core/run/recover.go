package run

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/loveRyujin/lantern/core/approval"
)

type disposition int

const (
	dispIllegal disposition = iota
	dispKeep
	dispExecuting
	dispBreakpoint
)

// classify 只看运行状态和序号最大的那一步。executing 优先于挂起。
func classify(state, stepStatus string) disposition {
	if stepStatus == StepExecuting {
		return dispExecuting
	}
	if state == StateAwaitingApproval {
		return dispKeep
	}
	if state == StateRunning && stepStatus == StepDone {
		return dispBreakpoint
	}
	return dispIllegal
}

// Recover 扫描未终结运行并按 FR-SM-5 归类。它不调用执行器。
//
// 本进程里用 Hold 登记过的运行（Resume 正在执行、Agent 循环正在跑）不归类、不写审计，
// 只记进 Report.Active。是否登记在每趟运行的写事务里判断：Resume 在第一次写库之前登记，
// 所以 Recover 要么看到登记而跳过，要么看到登记之前已提交的状态。
// 进程启动时登记表为空，同一个入口就是完整的启动扫描；工作区 Open（每次 Ask）也走这里。
func (s *Service) Recover(ctx context.Context) (Report, error) {
	if err := s.ready(ctx); err != nil {
		return Report{}, err
	}
	path, err := dbIdentity(ctx, s.db)
	if err != nil {
		return Report{}, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM runs
		WHERE state NOT IN (?, ?, ?, ?, ?)
		ORDER BY id`,
		StateCompleted, StateFailed, StateCancelled, StateRejected, StateExpired,
	)
	if err != nil {
		return Report{}, fmt.Errorf("run: 扫描运行: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return Report{}, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return Report{}, err
	}
	if err := rows.Close(); err != nil {
		return Report{}, err
	}

	var report Report
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		if err := s.recoverOne(ctx, path, id, &report); err != nil {
			return Report{}, err
		}
	}
	return report, nil
}

type outcome int

const (
	outNone outcome = iota
	outActive
	outInterrupted
	outIllegal
	outSuspended
	outBreakpoint
)

func (s *Service) recoverOne(ctx context.Context, path, id string, report *Report) error {
	var (
		out    outcome
		ticket approval.Ticket
	)
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		out = outNone
		rec, err := loadRun(ctx, tx, id)
		if err != nil {
			return err
		}
		if terminal(rec.state) {
			return nil
		}
		if held(path, id) {
			out = outActive
			return nil
		}
		stepID, stepStatus, err := lastStep(ctx, tx, id)
		if err != nil {
			return err
		}
		switch classify(rec.state, stepStatus) {
		case dispExecuting:
			out = outInterrupted
			return s.failTx(ctx, tx, rec, stepID, "", AuditExecutingInterrupted, "executing")
		case dispKeep:
			t, k, err := loadRestorable(ctx, tx, id)
			if err != nil {
				return err
			}
			if k == kindNone {
				out = outIllegal
				return s.failTx(ctx, tx, rec, stepID, "", AuditIllegalResidual, "no_restorable_approval")
			}
			out, ticket = outSuspended, t
			return insertAudit(ctx, tx, s.clock(), AuditApprovalRestored, id, auditDetail(t.ID, t.StepID, "awaiting_approval"))
		case dispBreakpoint:
			out = outBreakpoint
			return insertAudit(ctx, tx, s.clock(), AuditBreakpointHeld, id, auditDetail("", stepID, "running_done"))
		default:
			out = outIllegal
			return s.failTx(ctx, tx, rec, stepID, "", AuditIllegalResidual, "no_breakpoint")
		}
	})
	if err != nil {
		return err
	}
	switch out {
	case outActive:
		report.Active = append(report.Active, id)
	case outInterrupted:
		report.Interrupted = append(report.Interrupted, id)
	case outIllegal:
		report.Illegal = append(report.Illegal, id)
	case outSuspended:
		report.Suspended = append(report.Suspended, Suspended{RunID: id, Ticket: ticket})
	case outBreakpoint:
		report.Breakpoints = append(report.Breakpoints, id)
	}
	return nil
}

func (s *Service) failTx(ctx context.Context, tx *sql.Tx, rec runRow, stepID, ticketID, action, policy string) error {
	if err := casState(ctx, tx, rec.id, rec.state, StateFailed, rec.version, s.stamp()); err != nil {
		return err
	}
	return insertAudit(ctx, tx, s.clock(), action, rec.id, auditDetail(ticketID, stepID, policy))
}
