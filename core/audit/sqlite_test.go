package audit

import (
	"context"
	"strings"
	"testing"

	"github.com/loveRyujin/lantern/core/workspace"
)

func TestSQLiteAppendOnly(t *testing.T) {
	h, err := workspace.Create(t.Context(), t.TempDir(), "audit")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	log := NewSQLite(h.Runs)
	if err := log.Append(context.Background(), Record{
		Actor:  "agent",
		Action: "argument_failures_exhausted",
		RunID:  "run-1",
		Detail: "tool_arguments_invalid",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Runs.ExecContext(context.Background(), `UPDATE audit SET detail = '改掉'`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("更新审计应失败: %v", err)
	}
	if _, err := h.Runs.ExecContext(context.Background(), `DELETE FROM audit`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("删除审计应失败: %v", err)
	}
	var n int
	if err := h.Runs.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM audit`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("审计行数 %d", n)
	}
}
