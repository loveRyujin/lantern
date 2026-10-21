package workspace

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"unicode"
)

// exportGrant 是签发记录。同一张确认单在一个工作区里只能签发一次。
type exportGrant struct {
	approvalID string
	opening    bool
	used       bool
}

// ExportGrant 是用户确认一次敏感导出后签发的凭证。
// 只有 [Handle.ConfirmExport] 能签发。字段不导出，包外写不出一张能通过校验的凭证。
// 凭证绑在签发它的工作区上，只能成功打开一次 sensitive.db。
type ExportGrant struct {
	h          *Handle
	id         uint64
	approvalID string
}

// ApprovalID 返回签发这张凭证时记下的确认单标识。
func (g ExportGrant) ApprovalID() string { return g.approvalID }

// ConfirmExport 在用户已经确认这次导出之后签发凭证。
// approvalID 是确认单标识，同一工作区里同一张确认单只能签发一次。
// 本方法不创建、不打开 sensitive.db。
func (h *Handle) ConfirmExport(approvalID string) (ExportGrant, error) {
	if h == nil {
		return ExportGrant{}, fmt.Errorf("workspace: %w", ErrExportUnconfirmed)
	}
	if err := validateApprovalID(approvalID); err != nil {
		return ExportGrant{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ExportGrant{}, fmt.Errorf("workspace: closed: %w", ErrExportUnconfirmed)
	}
	for _, g := range h.grants {
		if g.approvalID == approvalID {
			return ExportGrant{}, fmt.Errorf("workspace: approval %q: %w", approvalID, ErrExportUnconfirmed)
		}
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
	if id == "" || len(id) > 256 {
		return fmt.Errorf("workspace: approval id: %w", ErrInvalidApproval)
	}
	for _, r := range id {
		if r == 0 || unicode.IsControl(r) || unicode.IsSpace(r) {
			return fmt.Errorf("workspace: approval id: %w", ErrInvalidApproval)
		}
	}
	return nil
}
