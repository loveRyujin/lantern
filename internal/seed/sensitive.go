package seed

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/loveRyujin/lantern/core/workspace"
)

// SeedApprovalID 是种子工具写入敏感库时准备使用的确认单标识。
// 目前只是占位，#18 合并后改为走 ConfirmExport。
// 业务导出不要复用这个标识。
const SeedApprovalID = "lantern-seed"

// writeSensitive 把手机号和邮箱写入 sensitive.db，并返回这条连接供内容哈希读取。
// 种子工具只有这个函数访问敏感库。连接由 Handle.Close 关闭，这里不关闭。
//
// 当前直接使用 Create 时已经打开的 Handle.Sensitive。
// 敏感库改为确认导出后才打开时，只改这个函数：
// ConfirmExport(SeedApprovalID) 拿到凭证，再 OpenSensitive。
func writeSensitive(ctx context.Context, h *workspace.Handle, rows []contact) (*sql.DB, error) {
	if SeedApprovalID == "" {
		return nil, errors.New("seed: 缺少敏感库确认单标识")
	}
	if h == nil || h.Sensitive == nil {
		return nil, errors.New("seed: 敏感库没有打开")
	}
	db := h.Sensitive
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
