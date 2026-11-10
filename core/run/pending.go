package run

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/loveRyujin/lantern/core/approval"
)

// ListPending 只读列出仍可处理的待确认单。
//
// 它不写审计，不改状态，也不调用 Recover。条件是：运行仍为 awaiting_approval，
// 确认单为 pending、未消费、未过期，工作区标识与传入的一致，且最后一步不是 executing。
// 风险列为空表示开票时没有冻结风险说明，原样返回，不在这里补。
func ListPending(ctx context.Context, db *sql.DB, workspaceID string, now time.Time) ([]approval.Ticket, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if db == nil {
		return nil, ErrNoDB
	}
	if workspaceID == "" {
		return nil, fmt.Errorf("run: 工作区标识为空")
	}
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	rows, err := db.QueryContext(ctx, `
		SELECT a.id, a.run_id, a.step_id, a.params_hash, a.params_json,
			a.workspace_id, a.action, a.export_object, a.expires_at, a.risk
		FROM approvals a
		JOIN runs r ON r.id = a.run_id
		WHERE r.state = ?
		  AND a.state = ?
		  AND a.workspace_id = ?
		  AND (a.consumed_at IS NULL OR a.consumed_at = '')
		  AND COALESCE((
			SELECT s.status FROM run_steps s
			WHERE s.run_id = r.id
			ORDER BY s.ordinal DESC
			LIMIT 1
		  ), '') != ?
		ORDER BY a.created_at, a.id`,
		StateAwaitingApproval, string(approval.StatePending), workspaceID, StepExecuting,
	)
	if err != nil {
		return nil, fmt.Errorf("run: 读取待确认: %w", err)
	}
	defer rows.Close()
	var out []approval.Ticket
	for rows.Next() {
		var (
			id, runID, stepID, hash, params, ws, action, object, expires, risk string
		)
		if err := rows.Scan(&id, &runID, &stepID, &hash, &params, &ws, &action, &object, &expires, &risk); err != nil {
			return nil, fmt.Errorf("run: 读取待确认: %w", err)
		}
		exp, ok := parseTime(expires)
		if !ok || !exp.After(now) {
			continue
		}
		out = append(out, approval.Ticket{
			ID:           id,
			RunID:        runID,
			StepID:       stepID,
			WorkspaceID:  ws,
			Action:       action,
			Params:       append([]byte(nil), params...),
			ParamsHash:   hash,
			ExportObject: object,
			ExpiresAt:    exp,
			Risk:         risk,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("run: 读取待确认: %w", err)
	}
	if out == nil {
		out = []approval.Ticket{}
	}
	return out, nil
}
