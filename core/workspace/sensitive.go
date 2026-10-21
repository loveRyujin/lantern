package workspace

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"time"
	"unicode"

	"github.com/loveRyujin/lantern/core/approval"
)

// actionExportData 是会打开敏感库的那一次工具调用。
// 写操作或其他确认单即使已批准，也不能用来签发导出凭证。
const actionExportData = "export_data"

// exportGrant 是签发记录。同一张确认单在一个工作区里只能签发一次。
type exportGrant struct {
	approvalID string
	opening    bool
	used       bool
}

// ExportGrant 是对照 runs.approvals 里一条已批准导出确认后签发的凭证。
// 只有 [Handle.ConfirmExport] 能签发。字段不导出，包外写不出一张能通过校验的凭证。
// 凭证绑在签发它的工作区上，只能成功打开一次 sensitive.db。
type ExportGrant struct {
	h          *Handle
	id         uint64
	approvalID string
}

// ApprovalID 返回这张凭证对应的确认单标识。
func (g ExportGrant) ApprovalID() string { return g.approvalID }

// ConfirmExport 在 runs.approvals 里找到一条可用的导出确认后签发凭证。
//
// 记录必须已经存在，并且同时满足：决定是批准、动作是 export_data、
// workspace_id 等于这个工作区、export_object 等于 exportObject、
// expires_at 晚于现在、consumed_at 为空。签发成功会写上 consumed_at，
// 所以同一张确认单不能再签发第二次。本方法不创建、不打开 sensitive.db。
// 任意字符串、未批准、已拒绝、别的工作区、别的导出对象、过期或重放都返回
// [ErrExportUnconfirmed]，错误码是 export_unconfirmed。
func (h *Handle) ConfirmExport(ctx context.Context, approvalID, exportObject string) (ExportGrant, error) {
	if err := ctx.Err(); err != nil {
		return ExportGrant{}, err
	}
	if h == nil || h.Runs == nil {
		return ExportGrant{}, ErrExportUnconfirmed
	}
	if err := validateApprovalID(approvalID); err != nil {
		return ExportGrant{}, err
	}
	if err := validateExportObject(exportObject); err != nil {
		return ExportGrant{}, err
	}
	h.mu.Lock()
	closed := h.closed
	h.mu.Unlock()
	if closed {
		return ExportGrant{}, fmt.Errorf("workspace: closed: %w", ErrExportUnconfirmed)
	}
	if err := h.consumeApproval(ctx, approvalID, exportObject); err != nil {
		return ExportGrant{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ExportGrant{}, fmt.Errorf("workspace: closed: %w", ErrExportUnconfirmed)
	}
	h.nextGrant++
	id := h.nextGrant
	if h.grants == nil {
		h.grants = make(map[uint64]*exportGrant)
	}
	h.grants[id] = &exportGrant{approvalID: approvalID}
	return ExportGrant{h: h, id: id, approvalID: approvalID}, nil
}

// OpenSensitive 用确认凭证打开 sensitive.db，返回带 ATTACH 防护的连接。
// 凭证为零、伪造、属于其他工作区、已经用过，或工作区已关闭时，返回 [ErrExportUnconfirmed]，
// 并且不会创建或打开文件。同一张凭证只能成功打开一次。
// 调用方可以 Close 返回的连接；[Handle.Close] 也会关掉还开着的连接。
func (h *Handle) OpenSensitive(ctx context.Context, grant ExportGrant) (*sql.DB, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := h.reserve(grant); err != nil {
		return nil, err
	}
	db, err := openDB(ctx, filepath.Join(h.Dir, fileSensitive), sensitiveMigrations, "migrations/sensitive", nil)
	if err != nil {
		h.release(grant.id)
		return nil, fmt.Errorf("workspace: sensitive: %w", err)
	}
	if err := h.commit(grant.id, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func (h *Handle) consumeApproval(ctx context.Context, approvalID, exportObject string) error {
	tx, err := h.Runs.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("workspace: approval: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var workspaceID, action, object, decision, expiresAt string
	var consumed sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT workspace_id, action, export_object, decision, expires_at, consumed_at
		FROM approvals WHERE id = ?`, approvalID,
	).Scan(&workspaceID, &action, &object, &decision, &expiresAt, &consumed)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrExportUnconfirmed
	}
	if err != nil {
		return fmt.Errorf("workspace: approval: %w", err)
	}
	if !approvalUsable(h.ID, exportObject, workspaceID, action, object, decision, expiresAt, consumed.Valid, time.Now()) {
		return ErrExportUnconfirmed
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := tx.ExecContext(ctx, `
		UPDATE approvals SET consumed_at = ?
		WHERE id = ? AND consumed_at IS NULL`, now, approvalID,
	)
	if err != nil {
		return fmt.Errorf("workspace: approval: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("workspace: approval: %w", err)
	}
	if n != 1 {
		return ErrExportUnconfirmed
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("workspace: approval: %w", err)
	}
	return nil
}

func approvalUsable(handleID, wantObject, workspaceID, action, object, decision, expiresAt string, consumed bool, now time.Time) bool {
	if workspaceID != handleID || action != actionExportData || object != wantObject {
		return false
	}
	if decision != string(approval.ActionApprove) || consumed {
		return false
	}
	exp, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		exp, err = time.Parse(time.RFC3339, expiresAt)
		if err != nil {
			return false
		}
	}
	return exp.After(now)
}

func (h *Handle) reserve(grant ExportGrant) error {
	if h == nil || grant.h != h || grant.id == 0 || grant.approvalID == "" {
		return fmt.Errorf("workspace: sensitive: %w", ErrExportUnconfirmed)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return fmt.Errorf("workspace: sensitive: closed: %w", ErrExportUnconfirmed)
	}
	g := h.grants[grant.id]
	if g == nil || g.used || g.opening || g.approvalID != grant.approvalID {
		return fmt.Errorf("workspace: sensitive: %w", ErrExportUnconfirmed)
	}
	g.opening = true
	return nil
}

func (h *Handle) release(id uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	g := h.grants[id]
	if g != nil && !g.used {
		g.opening = false
	}
}

func (h *Handle) commit(id uint64, db *sql.DB) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	g := h.grants[id]
	if h.closed || g == nil || g.used || !g.opening {
		return fmt.Errorf("workspace: sensitive: %w", ErrExportUnconfirmed)
	}
	g.opening = false
	g.used = true
	h.sensitive = append(h.sensitive, db)
	return nil
}

func validateApprovalID(id string) error {
	if err := validateToken(id); err != nil {
		return fmt.Errorf("workspace: approval id: %w", errors.Join(ErrExportUnconfirmed, ErrInvalidApproval))
	}
	return nil
}

func validateExportObject(object string) error {
	if err := validateToken(object); err != nil {
		return fmt.Errorf("workspace: export object: %w", errors.Join(ErrExportUnconfirmed, ErrInvalidApproval))
	}
	return nil
}

func validateToken(id string) error {
	if id == "" || len(id) > 256 {
		return ErrInvalidApproval
	}
	for _, r := range id {
		if r == 0 || unicode.IsControl(r) || unicode.IsSpace(r) {
			return ErrInvalidApproval
		}
	}
	return nil
}
