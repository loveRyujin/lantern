package seed

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/loveRyujin/lantern/core/approval"
	"github.com/loveRyujin/lantern/core/workspace"
)

// seedApprovalPrefix 是种子确认单标识的前缀。每次写入都另加一段随机后缀，
// 避免同一工作区再次生成时主键冲突。业务导出不要复用这个前缀。
const seedApprovalPrefix = "lantern-seed-"

// seedExportObject 是种子写入的导出对象，对应 sensitive.user_contacts。
const seedExportObject = "user_contacts"

// writeSensitive 把手机号和邮箱写入 sensitive.db，并返回这条连接供内容哈希读取。
// 种子工具只有这个函数访问敏感库。连接由 Handle.Close 关闭，这里不关闭。
//
// 先在本工作区的 runs.approvals 写入一条已批准、未过期的 export_data 确认，
// 再 ConfirmExport。没有这条记录就拿不到连接。每次调用使用新的确认单标识。
func writeSensitive(ctx context.Context, h *workspace.Handle, rows []contact) (*sql.DB, error) {
	if h == nil {
		return nil, errors.New("seed: 工作区没有打开")
	}
	approvalID, err := writeSeedApproval(ctx, h)
	if err != nil {
		return nil, err
	}
	cc, err := SeedConsumeContext(approvalID, seedExportObject)
	if err != nil {
		return nil, err
	}
	grant, err := h.ConfirmExport(ctx, approvalID, cc, seedExportObject)
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

// RecordExportApproval 为种子准备一张已经批准的导出确认。
// 只供 internal/seed 和它的测试使用，别的包不能调用。
//
// 确认单的插入和批准只走审批状态机。运行行不是审批决定，所以仍由种子写入，
// 并且和确认单放在同一个事务里，失败就一起回滚。
// 运行标识是 seed-<approvalID>，步骤是 seed。一小时内有效。
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
	ticket, err := seedTicket(h.ID, approvalID, exportObject, time.Now().UTC())
	if err != nil {
		return err
	}
	machine := approval.New(h.Runs)
	err = machine.Transact(ctx, func(tx *sql.Tx) error {
		created := ticket.ExpiresAt.Add(-time.Hour).UTC().Format(time.RFC3339Nano)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO runs (id, state, version, context, created_at, updated_at)
			VALUES (?, 'completed', 0, '', ?, ?)`, ticket.RunID, created, created); err != nil {
			return fmt.Errorf("seed: 写入种子运行: %w", err)
		}
		if err := machine.CreateTx(ctx, tx, ticket); err != nil {
			return err
		}
		if err := machine.DecideTx(ctx, tx, approvalID, approval.Decision{Action: approval.ActionApprove}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("seed: 写入导出确认: %w", err)
	}
	return nil
}

// SeedConsumeContext 按种子确认单的约定拼出消费上下文。
// 运行标识是 seed-<approvalID>，步骤是 seed，哈希和 RecordExportApproval 用的是同一份参数。
func SeedConsumeContext(approvalID, exportObject string) (approval.ConsumeContext, error) {
	ticket, err := seedTicket("workspace", approvalID, exportObject, time.Now().UTC())
	if err != nil {
		return approval.ConsumeContext{}, err
	}
	return approval.ConsumeContext{RunID: ticket.RunID, StepID: ticket.StepID, ArgsHash: ticket.ParamsHash}, nil
}

func seedTicket(workspaceID, approvalID, exportObject string, now time.Time) (approval.Ticket, error) {
	params, err := json.Marshal(map[string]string{
		"action": "export_data",
		"object": exportObject,
	})
	if err != nil {
		return approval.Ticket{}, fmt.Errorf("seed: 确认单参数: %w", err)
	}
	hash, err := approval.ParamsHash(params)
	if err != nil {
		return approval.Ticket{}, fmt.Errorf("seed: 确认单哈希: %w", err)
	}
	return approval.Ticket{
		ID:           approvalID,
		RunID:        "seed-" + approvalID,
		StepID:       "seed",
		WorkspaceID:  workspaceID,
		Action:       "export_data",
		ExportObject: exportObject,
		Params:       params,
		ParamsHash:   hash,
		ExpiresAt:    now.Add(time.Hour),
	}, nil
}

// writeSeedApproval 为这次种子写入签发一张新的确认单，并记入 runs.approvals。
func writeSeedApproval(ctx context.Context, h *workspace.Handle) (string, error) {
	id, err := newSeedApprovalID()
	if err != nil {
		return "", err
	}
	if err := RecordExportApproval(ctx, h, id, seedExportObject); err != nil {
		return "", err
	}
	return id, nil
}

func newSeedApprovalID() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("seed: 确认单标识: %w", err)
	}
	return seedApprovalPrefix + hex.EncodeToString(buf[:]), nil
}

func validSeedToken(s string) bool {
	if s == "" || len(s) > 256 {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool {
		return r == 0 || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
}
