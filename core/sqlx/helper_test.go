package sqlx_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/loveRyujin/lantern/core/sqlx"
	"modernc.org/sqlite"
)

type drvConnector struct {
	dsn string
	drv *sqlite.Driver
}

func (c drvConnector) Connect(ctx context.Context) (driver.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.drv.Open(c.dsn)
}

func (c drvConnector) Driver() driver.Driver { return c.drv }

func openWritable(t *testing.T, path, journal string) *sql.DB {
	t.Helper()
	u := url.URL{
		Scheme: "file",
		Path:   filepath.ToSlash(path),
		RawQuery: url.Values{
			"_busy_timeout": {"5000"},
			"_journal_mode": {journal},
		}.Encode(),
	}
	db := sql.OpenDB(drvConnector{dsn: u.String(), drv: &sqlite.Driver{}})
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	return db
}

func seedTable(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := t.Context()
	if _, err := db.ExecContext(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO t (id, name) VALUES (1, 'a')`); err != nil {
		t.Fatal(err)
	}
}

func removeSidecars(path string) {
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
	_ = os.Remove(path + "-journal")
}

type sideInfo struct {
	exists bool
	sum    [32]byte
}

type finger struct {
	resolved string
	main     [32]byte
	sides    map[string]sideInfo
	snap     sqlx.Snap
}

func takeFinger(t *testing.T, path string) finger {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := sqlx.Snapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Resolved != resolved {
		t.Fatalf("Snapshot 路径 %s，EvalSymlinks 之后是 %s", snap.Resolved, resolved)
	}
	sides := map[string]sideInfo{}
	for _, p := range []string{resolved + "-wal", abs + "-wal"} {
		sides[p] = hashMaybe(t, p)
	}
	return finger{resolved: resolved, main: hashFile(t, resolved), sides: sides, snap: snap}
}

func (before finger) assertSame(t *testing.T, path string) {
	t.Helper()
	after := takeFinger(t, path)
	if before.resolved != after.resolved {
		t.Fatalf("解析后的路径变了 %s → %s", before.resolved, after.resolved)
	}
	if before.main != after.main {
		t.Fatal("主库文件哈希变了")
	}
	for p, b := range before.sides {
		a, ok := after.sides[p]
		if !ok {
			t.Fatalf("少看了 %s", p)
		}
		if !b.exists && a.exists {
			t.Fatalf("执行前没有 -wal，执行后出现了 %s", p)
		}
		if b.exists && (!a.exists || a.sum != b.sum) {
			t.Fatalf("-wal 变了 %s", p)
		}
	}
	if err := before.snap.Unchanged(after.snap); err != nil {
		t.Fatal(err)
	}
}

func hashFile(t *testing.T, path string) [32]byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(body)
}

func hashMaybe(t *testing.T, path string) sideInfo {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return sideInfo{}
		}
		t.Fatal(err)
	}
	return sideInfo{exists: true, sum: sha256.Sum256(body)}
}

type spyDB struct {
	*sql.DB
	n int
}

func (s *spyDB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	s.n++
	return s.DB.QueryContext(ctx, query, args...)
}
