//go:build linux || darwin

package sqlx

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSQLProcessAcceptsEquivalentSymlinkPath(t *testing.T) {
	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(realDir, "analytics.db")
	createEmptyDB(t, path)
	linkDir := filepath.Join(base, "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatal(err)
	}
	via := filepath.Join(linkDir, "analytics.db")
	res, stats, err := runSQLProcess(t.Context(), processRequest{Path: via, Allowed: via, SQL: "SELECT 1", Limit: 1})
	if err != nil || res.RowCount != 1 {
		t.Fatalf("symlink spelling: result=%+v stats=%+v error=%v", res, stats, err)
	}
	if stats.MemoryLimit != ProcessMemoryBytes {
		t.Fatalf("worker did not confirm the memory cap: %+v", stats)
	}
}
