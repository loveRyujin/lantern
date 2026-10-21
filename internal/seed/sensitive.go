package seed

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	if err := insertSeedApproval(ctx, h); err != nil {
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

func insertSeedApproval(ctx context.Context, h *workspace.Handle) error {
	params := []byte(`{"action":"export_data","object":"user_contacts"}`)
	hash, err := approval.ParamsHash(params)
	if err != nil {
		return fmt.Errorf("seed: 确认单哈希: %w", err)
	}
	now := time.Now().UTC()
	created := now.Format(time.RFC3339Nano)
	expires := now.Add(time.Hour).Format(time.RFC3339Nano)
	if _, err := h.Runs.ExecContext(ctx, `
		INSERT INTO runs (id, state, version, context, created_at, updated_at)
		VALUES ('seed', 'completed', 0, '', ?, ?)`, created, created); err != nil {
		return fmt.Errorf("seed: 写入种子运行: %w", err)
	}
	if _, err := h.Runs.ExecContext(ctx, `
		INSERT INTO approvals (
			id, run_id, step_id, params_hash, params_json, decision, created_at, decided_at,
			workspace_id, action, export_object, expires_at
		) VALUES (?, 'seed', 'seed', ?, ?, ?, ?, ?, ?, 'export_data', ?, ?)`,
		SeedApprovalID, hash, string(params), string(approval.ActionApprove), created, created,
		h.ID, seedExportObject, expires,
	); err != nil {
		return fmt.Errorf("seed: 写入导出确认: %w", err)
	}
	return nil
}
