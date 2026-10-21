package seed

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/loveRyujin/lantern/core/approval"
	"github.com/loveRyujin/lantern/core/workspace"
)

// SeedApprovalID 是种子工具写入敏感库时使用的确认单标识。
// 业务导出不要复用这个标识。
const SeedApprovalID = "lantern-seed"

// seedExportObject 是种子写入的导出对象，对应 sensitive.user_contacts。
const seedExportObject = "user_contacts"

// writeSensitive 把手机号和邮箱写入 sensitive.db，并返回这条连接供内容哈希读取。
// 种子工具只有这个函数访问敏感库。连接由 Handle.Close 关闭，这里不关闭。
//
// 先在本工作区的 runs.approvals 写入一条已批准、未过期的 export_data 确认，
// 再 ConfirmExport。没有这条记录就拿不到连接。
func writeSensitive(ctx context.Context, h *workspace.Handle, rows []contact) (*sql.DB, error) {
	if SeedApprovalID == "" {
		return nil, errors.New("seed: 缺少敏感库确认单标识")
	}
	if h == nil {
		return nil, errors.New("seed: 工作区没有打开")
	}
	if err := RecordExportApproval(ctx, h, SeedApprovalID, seedExportObject); err != nil {
		return nil, err
	}
	grant, err := h.ConfirmExport(ctx, SeedApprovalID, seedExportObject)
	if err != nil {
		return nil, fmt.Errorf("seed: 确认导出: %w", err)
	}
	db, err := h.OpenSensitive(ctx, grant)
	if err != nil {
		return nil, fmt.Errorf("seed: 打开敏感库: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := insertContacts(ctx, db, rows); err != nil {
		return nil, err
	}
	if err := checkpoint(ctx, db); err != nil {
		return nil, err
	}
	return db, nil
}

func insertContacts(ctx context.Context, db *sql.DB, rows []contact) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("seed: 敏感库事务: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO user_contacts (user_id, phone, email) VALUES (?, ?, ?)
	`)
	if err != nil {
		return fmt.Errorf("seed: 准备敏感库写入: %w", err)
	}
	defer stmt.Close()

	for _, row := range rows {
		if row.UserID == "" || row.Phone == "" || row.Email == "" {
			return errors.New("seed: 敏感列为空")
		}
		if _, err := stmt.ExecContext(ctx, row.UserID, row.Phone, row.Email); err != nil {
			return fmt.Errorf("seed: 写入敏感列 %s: %w", row.UserID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("seed: 提交敏感库: %w", err)
	}
	return nil
}

// RecordExportApproval 在这个工作区的 runs.approvals 写入一张尚未使用的导出确认。
// 记录属于 h 的工作区，动作是 export_data，对象是 exportObject，决定为批准，一小时内有效。
// 不打开 sensitive.db，也不调用 ConfirmExport。调用方拿到记录后再自己确认。
func RecordExportApproval(ctx context.Context, h *workspace.Handle, approvalID, exportObject string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if h == nil || h.Runs == nil {
		return errors.New("seed: 工作区没有打开")
	}
	if !validSeedToken(approvalID) || !validSeedToken(exportObject) {
		return errors.New("seed: 确认单标识或导出对象不合法")
	}
	params, err := json.Marshal(map[string]string{
		"action": "export_data",
		"object": exportObject,
	})
	if err != nil {
		return fmt.Errorf("seed: 确认单参数: %w", err)
	}
	hash, err := approval.ParamsHash(params)
	if err != nil {
		return fmt.Errorf("seed: 确认单哈希: %w", err)
	}
	now := time.Now().UTC()
	created := now.Format(time.RFC3339Nano)
	expires := now.Add(time.Hour).Format(time.RFC3339Nano)
	runID := "seed-" + approvalID
	if _, err := h.Runs.ExecContext(ctx, `
		INSERT INTO runs (id, state, version, context, created_at, updated_at)
		VALUES (?, 'completed', 0, '', ?, ?)`, runID, created, created); err != nil {
		return fmt.Errorf("seed: 写入种子运行: %w", err)
	}
	if _, err := h.Runs.ExecContext(ctx, `
		INSERT INTO approvals (
			id, run_id, step_id, params_hash, params_json, decision, created_at, decided_at,
			workspace_id, action, export_object, expires_at
		) VALUES (?, ?, 'seed', ?, ?, ?, ?, ?, ?, 'export_data', ?, ?)`,
		approvalID, runID, hash, string(params), string(approval.ActionApprove), created, created,
		h.ID, exportObject, expires,
	); err != nil {
		return fmt.Errorf("seed: 写入导出确认: %w", err)
	}
	return nil
}

func validSeedToken(s string) bool {
	if s == "" || len(s) > 256 {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool {
		return r == 0 || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
}
