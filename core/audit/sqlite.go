package audit

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// SQLite 把审计追加进 runs.db 的 audit 表。
// 表上的触发器拒绝更新和删除，这里也不提供这两条路径。
type SQLite struct {
	db *sql.DB
}

// NewSQLite 使用已经打开的 runs 库。db 为 nil 时 Append 返回错误。
func NewSQLite(db *sql.DB) *SQLite {
	return &SQLite{db: db}
}

// Append 只 INSERT。Detail 带上 run_id，因为这张表没有单独的 run_id 列。
func (s *SQLite) Append(ctx context.Context, rec Record) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("audit: 没有数据库")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	at := rec.At
	if at.IsZero() {
		at = time.Now()
	}
	actor := rec.Actor
	if actor == "" {
		actor = "agent"
	}
	detail := rec.Detail
	if rec.RunID != "" {
		detail = rec.RunID + ": " + detail
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit (at, actor, action, detail) VALUES (?, ?, ?, ?)`,
		at.UTC().Format(time.RFC3339Nano), actor, rec.Action, detail,
	)
	if err != nil {
		return fmt.Errorf("audit: 追加失败: %w", err)
	}
	return nil
}
