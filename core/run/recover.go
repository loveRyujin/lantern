package run

import (
	"context"
	"database/sql"
	"fmt"
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
func (s *Service) Recover(ctx context.Context) (Report, error) {
	if err := s.ready(ctx); err != nil {
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
		if err := s.recoverOne(ctx, id, &report); err != nil {
			return Report{}, err
		}
	}
	return report, nil
}

func (s *Service) recoverOne(ctx context.Context, id string, report *Report) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	rec, err := loadRun(ctx, tx, id)
	if err != nil {
		return err
	}
	if terminal(rec.state) {
		return tx.Commit()
	}
	stepID, stepStatus, err := lastStep(ctx, tx, id)
	if err != nil {
		return err
	}
	switch classify(rec.state, stepStatus) {
	case dispExecuting:
		if err := s.failTx(ctx, tx, rec, stepID, "", AuditExecutingInterrupted, "executing"); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		report.Interrupted = append(report.Interrupted, id)
		return nil
	case dispKeep:
		ticket, k, err := loadRestorable(ctx, tx, id)
		if err != nil {
			return err
		}
		if k == kindNone {
			if err := s.failTx(ctx, tx, rec, stepID, "", AuditIllegalResidual, "no_restorable_approval"); err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return err
			}
			report.Illegal = append(report.Illegal, id)
			return nil
		}
		if err := insertAudit(ctx, tx, s.clock(), AuditApprovalRestored, id, auditDetail(ticket.ID, ticket.StepID, "awaiting_approval")); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		report.Suspended = append(report.Suspended, Suspended{RunID: id, Ticket: ticket})
		return nil
	case dispBreakpoint:
		if err := insertAudit(ctx, tx, s.clock(), AuditBreakpointHeld, id, auditDetail("", stepID, "running_done")); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		report.Breakpoints = append(report.Breakpoints, id)
		return nil
	default:
		if err := s.failTx(ctx, tx, rec, stepID, "", AuditIllegalResidual, "no_breakpoint"); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		report.Illegal = append(report.Illegal, id)
		return nil
	}
}

func (s *Service) failTx(ctx context.Context, tx *sql.Tx, rec runRow, stepID, ticketID, action, policy string) error {
	if err := casState(ctx, tx, rec.id, rec.state, StateFailed, rec.version, s.stamp()); err != nil {
		return err
	}
	return insertAudit(ctx, tx, s.clock(), action, rec.id, auditDetail(ticketID, stepID, policy))
}
