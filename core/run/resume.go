package run

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/loveRyujin/lantern/core/approval"
	"github.com/loveRyujin/lantern/core/tools"
)

// Resume 对一趟仍挂起的运行重新征求决定，或完成已经批准但还没消费的确认单。
// 工具只在哈希核对和状态机消费都成功之后执行一次，参数用确认单里的冻结快照。
func (s *Service) Resume(ctx context.Context, runID string) (ResumeResult, error) {
	if err := s.ready(ctx); err != nil {
		return ResumeResult{}, err
	}
	if runID == "" {
		return ResumeResult{}, errors.New("run: 运行标识为空")
	}
	if s.machine == nil {
		return ResumeResult{}, ErrNoDB
	}
	release, err := Hold(ctx, s.db, runID)
	if err != nil {
		return ResumeResult{}, err
	}
	defer release()

	rec, err := loadRun(ctx, s.db, runID)
	if err != nil {
		return ResumeResult{}, err
	}
	if rec.state != StateAwaitingApproval {
		return ResumeResult{State: rec.state}, ErrNotAwaiting
	}
	stepID, stepStatus, err := lastStep(ctx, s.db, runID)
	if err != nil {
		return ResumeResult{}, err
	}
	if stepStatus == StepExecuting {
		if err := s.failRun(ctx, runID, stepID, "", AuditExecutingInterrupted, "executing"); err != nil {
			return ResumeResult{}, err
		}
		return ResumeResult{State: StateFailed}, ErrInterrupted
	}

	ticket, k, err := loadRestorable(ctx, s.db, runID)
	if err != nil {
		return ResumeResult{}, err
	}
	if k == kindNone {
		if err := s.failRun(ctx, runID, stepID, "", AuditIllegalResidual, "no_restorable_approval"); err != nil {
			return ResumeResult{}, err
		}
		return ResumeResult{State: StateFailed}, ErrNotAwaiting
	}

	if k == kindPending {
		if s.approver == nil {
			return ResumeResult{State: StateAwaitingApproval}, ErrNoApprover
		}
		decision, err := s.approver.Request(ctx, ticket)
		if err != nil {
			return ResumeResult{State: StateAwaitingApproval}, err
		}
		switch decision.Action {
		case approval.ActionDefer:
			return ResumeResult{State: StateAwaitingApproval}, nil
		case approval.ActionReject:
			if _, err := s.machine.Decide(ctx, ticket.ID, decision); err != nil {
				return ResumeResult{}, err
			}
			if err := s.forceState(ctx, runID, StateRejected); err != nil {
				return ResumeResult{}, err
			}
			return ResumeResult{State: StateRejected}, nil
		case approval.ActionApprove:
			if decision.OverridesSnapshot() {
				sum, herr := approval.ParamsHash(decision.Params)
				if herr != nil || sum != ticket.ParamsHash {
					return s.refuseSnapshot(ctx, runID, ticket.StepID)
				}
			}
		default:
			return ResumeResult{}, fmt.Errorf("run: 确认决定不合法")
		}
	}

	fresh, _, err := loadRestorable(ctx, s.db, runID)
	if err != nil {
		return ResumeResult{}, err
	}
	if fresh.ID == "" {
		return ResumeResult{State: StateAwaitingApproval}, ErrNotAwaiting
	}
	ok, err := approval.SnapshotMatches(fresh.ParamsHash, fresh.Params)
	if err != nil || !ok {
		return s.refuseSnapshot(ctx, runID, fresh.StepID)
	}
	if s.execute == nil {
		return ResumeResult{State: StateAwaitingApproval}, ErrNoExecutor
	}

	if k == kindPending {
		if _, err := s.machine.Decide(ctx, fresh.ID, approval.Decision{Action: approval.ActionApprove}); err != nil {
			return s.decisionFailed(ctx, runID, err)
		}
	}
	if err := s.machine.Consume(ctx, fresh.ID, approval.ConsumeContext{
		RunID:    fresh.RunID,
		StepID:   fresh.StepID,
		ArgsHash: fresh.ParamsHash,
	}); err != nil {
		return s.decisionFailed(ctx, runID, err)
	}

	if err := s.Begin(ctx, fresh.StepID, fresh.RunID, fresh.Action, fresh.Params); err != nil {
		var exists *stepExistsError
		if errors.As(err, &exists) {
			return s.stepAlreadyThere(ctx, runID, fresh.StepID, exists.Status)
		}
		// 确认单已经消费。步骤没落成 executing 时也不能再执行。
		_ = s.forceState(context.WithoutCancel(ctx), runID, StateFailed)
		return ResumeResult{State: StateFailed}, err
	}
	if err := s.Set(ctx, runID, StateRunning); err != nil {
		return ResumeResult{}, err
	}

	call := tools.Call{
		ID:          fresh.StepID,
		Name:        fresh.Action,
		Args:        append([]byte(nil), fresh.Params...),
		Hash:        fresh.ParamsHash,
		RunID:       fresh.RunID,
		WorkspaceID: fresh.WorkspaceID,
		ApprovalID:  fresh.ID,
		StepID:      fresh.StepID,
		ArgsHash:    fresh.ParamsHash,
	}
	statusCtx := context.WithoutCancel(ctx)
	started := s.clock()
	result, execErr := s.execute(ctx, call)
	_ = s.stepEvent(statusCtx, call, fresh.ID, result.Summary, s.clock().Sub(started), execErr)
	if execErr != nil {
		_ = s.Finish(statusCtx, fresh.StepID, StepError)
		_ = s.Set(statusCtx, runID, StateFailed)
		_ = s.writeAudit(statusCtx, runID, AuditToolFailed, auditDetail(fresh.ID, fresh.StepID, "tool_failed"))
		return ResumeResult{State: StateFailed, Executed: true}, execErr
	}
	if err := s.Finish(statusCtx, fresh.StepID, StepDone); err != nil {
		_ = s.Set(statusCtx, runID, StateFailed)
		return ResumeResult{State: StateFailed, Executed: true}, err
	}
	if err := s.Set(statusCtx, runID, StateCompleted); err != nil {
		return ResumeResult{State: StateRunning, Executed: true}, err
	}
	return ResumeResult{State: StateCompleted, Executed: true}, nil
}

func (s *Service) decisionFailed(ctx context.Context, runID string, err error) (ResumeResult, error) {
	switch {
	case errors.Is(err, approval.ErrExpired):
		if serr := s.forceState(ctx, runID, StateExpired); serr != nil {
			return ResumeResult{}, serr
		}
		return ResumeResult{State: StateExpired}, err
	case errors.Is(err, approval.ErrSnapshotMismatch):
		return ResumeResult{State: StateFailed}, err
	default:
		return ResumeResult{}, err
	}
}

func (s *Service) refuseSnapshot(ctx context.Context, runID, stepID string) (ResumeResult, error) {
	if err := s.failRun(ctx, runID, stepID, "", AuditHashMismatch, "hash_mismatch"); err != nil {
		return ResumeResult{}, err
	}
	return ResumeResult{State: StateFailed}, hashMismatch{}
}

func (s *Service) stepAlreadyThere(ctx context.Context, runID, stepID, status string) (ResumeResult, error) {
	switch status {
	case StepDone:
		if err := s.forceState(ctx, runID, StateCompleted); err != nil {
			return ResumeResult{}, err
		}
		return ResumeResult{State: StateCompleted}, nil
	case StepError:
		if err := s.forceState(ctx, runID, StateFailed); err != nil {
			return ResumeResult{}, err
		}
		return ResumeResult{State: StateFailed}, nil
	default:
		if err := s.failRun(ctx, runID, stepID, "", AuditExecutingInterrupted, "executing"); err != nil {
			return ResumeResult{}, err
		}
		return ResumeResult{State: StateFailed}, ErrInterrupted
	}
}

// failRun 把未终结的运行标成 failed 并写审计。已经终结时什么都不写，返回 nil。
func (s *Service) failRun(ctx context.Context, runID, stepID, ticketID, action, policy string) error {
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		rec, err := loadRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		if terminal(rec.state) {
			return nil
		}
		return s.failTx(ctx, tx, rec, stepID, ticketID, action, policy)
	})
}

// forceState 和 Set 一样受终态保护：已终结的运行只能"改成"同一个终态（空操作）。
func (s *Service) forceState(ctx context.Context, runID, to string) error {
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		rec, err := loadRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		if rec.state == to {
			return nil
		}
		if terminal(rec.state) {
			return &TransitionError{RunID: runID, From: rec.state, To: to}
		}
		return casState(ctx, tx, rec.id, rec.state, to, rec.version, s.stamp())
	})
}

// stepEvent 给恢复后的那一次执行补一条 run_events，格式与 Agent 循环的工具记录一致。
// Summary 来自工具，不放参数原文。
func (s *Service) stepEvent(ctx context.Context, call tools.Call, ticketID, summary string, dur time.Duration, execErr error) error {
	p := tracePayload{
		ToolName:   call.Name,
		ParamsHash: call.Hash,
		Summary:    clipSummary(summary),
		DurationNS: dur.Nanoseconds(),
		TicketID:   ticketID,
		StepID:     call.StepID,
	}
	kind := "step"
	if execErr != nil {
		code, _ := tools.CodeOf(execErr)
		if code == "" {
			code = AuditToolFailed
		}
		kind, p.Status, p.Code, p.Summary = code, "failed", code, code
	}
	payload, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO run_events (run_id, kind, payload, created_at) VALUES (?, ?, ?, ?)`,
		call.RunID, kind, string(payload), s.stamp(),
	)
	return err
}

func clipSummary(s string) string {
	const max = 200
	if s == "" {
		return "执行完成"
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

func (s *Service) writeAudit(ctx context.Context, runID, action, detail string) error {
	return insertAudit(ctx, s.db, s.clock(), action, runID, detail)
}
