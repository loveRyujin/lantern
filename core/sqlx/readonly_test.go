package sqlx_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/sqlx"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func TestReadOnlyConnectionRejectsWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "analytics.db")
	w := openWritable(t, path, "DELETE")
	seedTable(t, w)
	if _, err := w.ExecContext(t.Context(), `CREATE INDEX idx_t_name ON t (name)`); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	removeSidecars(path)
	if _, err := os.Stat(path + "-wal"); !os.IsNotExist(err) {
		t.Fatal("前置条件应没有 -wal")
	}

	ctx := t.Context()
	db := openRO(t, path)
	writes := []struct {
		id  string
		sql string
	}{
		{"U-34-01-insert", `INSERT INTO t (id, name) VALUES (2, 'b')`},
		{"U-34-02-update", `UPDATE t SET name = 'z' WHERE id = 1`},
		{"U-34-03-delete", `DELETE FROM t WHERE id = 1`},
		{"U-34-04-create", `CREATE TABLE other (id INTEGER)`},
		{"U-34-05-drop", `DROP TABLE t`},
		{"U-34-06-replace", `REPLACE INTO t (id, name) VALUES (1, 'z')`},
		{"U-34-06-upsert", `INSERT INTO t (id, name) VALUES (1, 'z') ON CONFLICT(id) DO UPDATE SET name = 'z'`},
		{"U-34-06-index", `CREATE INDEX idx_t ON t (name)`},
		{"U-34-06-view", `CREATE VIEW v AS SELECT * FROM t`},
		{"U-34-06-trigger", `CREATE TRIGGER tr AFTER INSERT ON t BEGIN SELECT 1; END`},
		{"U-34-06-alter", `ALTER TABLE t ADD COLUMN extra TEXT`},
		{"U-34-06-temp", `CREATE TEMP TABLE tmp (id INTEGER)`},
		{"U-34-06-vacuum", `VACUUM`},
		{"U-34-06-reindex", `REINDEX`},
		{"U-34-06-analyze", `ANALYZE`},
		{"cte-delete", `WITH x AS (DELETE FROM t RETURNING id) SELECT * FROM x`},
		{"cte-insert", `WITH x AS (SELECT 2 AS id, 'b' AS name) INSERT INTO t (id, name) SELECT id, name FROM x`},
		{"multi-write-first", `DELETE FROM t WHERE id = 1; SELECT 1`},
		{"multi-write-second", `SELECT 1; DELETE FROM t WHERE id = 1`},
		{"load-extension", `SELECT load_extension('x')`},
		{"writefile", `SELECT writefile('x', 'y')`},
		{"readfile", `SELECT readfile('x')`},
	}
	for _, tc := range writes {
		t.Run(tc.id, func(t *testing.T) {
			before := takeFinger(t, path)
			if _, err := db.ExecContext(ctx, tc.sql); err == nil {
				t.Fatal("写入应当被拒绝")
			}
			before.assertSame(t, path)
		})
	}

	t.Run("U-34-05b-select-no-wal", func(t *testing.T) {
		before := takeFinger(t, path)
		if !walAbsent(before) {
			t.Fatal("执行前应没有 -wal")
		}
		var name string
		if err := db.QueryRowContext(ctx, `SELECT name FROM t WHERE id = 1`).Scan(&name); err != nil {
			t.Fatal(err)
		}
		if name != "a" {
			t.Fatalf("读到 %q", name)
		}
		before.assertSame(t, path)
		if _, err := os.Stat(path + "-wal"); !os.IsNotExist(err) {
			t.Fatal("合法 SELECT 新建了 -wal")
		}
	})
}

func TestReadOnlyKeepsExistingWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "analytics.db")
	w := openWritable(t, path, "WAL")
	ctx := t.Context()
	if _, err := w.ExecContext(ctx, `PRAGMA wal_autocheckpoint = 0`); err != nil {
		t.Fatal(err)
	}
	seedTable(t, w)
	info, err := os.Stat(path + "-wal")
	if err != nil {
		t.Fatalf("需要一份非空 -wal: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("需要一份非空 -wal")
	}
	before := takeFinger(t, path)
	if walAbsent(before) {
		t.Fatal("执行前应已有 -wal")
	}
	db := openRO(t, path)
	var name string
	if err := db.QueryRowContext(ctx, `SELECT name FROM t WHERE id = 1`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "a" {
		t.Fatalf("没读到已提交的 WAL 数据: %q", name)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO t (id, name) VALUES (2, 'b')`); err == nil {
		t.Fatal("WAL 库上的写入应当被拒绝")
	}
	_, _ = db.ExecContext(ctx, `PRAGMA journal_mode = DELETE`)
	var mode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if strings.EqualFold(mode, "DELETE") {
		t.Fatal("只读连接把日志模式改成了 DELETE")
	}
	before.assertSame(t, path)
}

func TestReadOnlyPragmaCannotEnableWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "analytics.db")
	w := openWritable(t, path, "DELETE")
	seedTable(t, w)
	_ = w.Close()
	removeSidecars(path)
	ctx := t.Context()
	db := openRO(t, path)
	pragmas := []string{
		`PRAGMA query_only = OFF`,
		`PRAGMA query_only = 0`,
		`PRAGMA query_only = false`,
		`PRAGMA main.query_only = OFF`,
		`PRAGMA writable_schema = ON`,
		`PRAGMA journal_mode = DELETE`,
		`PRAGMA temp_store = MEMORY`,
		`PRAGMA locking_mode = NORMAL`,
	}
	for _, stmt := range pragmas {
		t.Run(stmt, func(t *testing.T) {
			before := takeFinger(t, path)
			_, _ = db.ExecContext(ctx, stmt)
			if _, err := db.ExecContext(ctx, `INSERT INTO t (id, name) VALUES (2, 'b')`); err == nil {
				t.Fatal("PRAGMA 之后写入仍应失败")
			}
			before.assertSame(t, path)
		})
	}
	t.Run("fresh-connection-still-query-only", func(t *testing.T) {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		assertConnReadOnly(t, ctx, conn)
	})
}

func TestReadOnlyRejectsAttach(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "LEAKMARKER")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "analytics.db")
	w := openWritable(t, path, "DELETE")
	seedTable(t, w)
	_ = w.Close()
	removeSidecars(path)
	sens := filepath.Join(dir, "sensitive.db")
	ctx := t.Context()
	db := openRO(t, path)

	t.Run("U-34-07-missing-not-created", func(t *testing.T) {
		if _, err := os.Stat(sens); !os.IsNotExist(err) {
			t.Fatal(err)
		}
		before := takeFinger(t, path)
		_, err := db.ExecContext(ctx, `ATTACH DATABASE '`+sqlQuote(sens)+`' AS s`)
		assertAttachDenied(t, err)
		if _, statErr := os.Stat(sens); !os.IsNotExist(statErr) {
			t.Fatal("ATTACH 把 sensitive.db 创建出来了")
		}
		before.assertSame(t, path)
		assertPathNotOpen(t, sens)
	})

	t.Run("U-34-08-variants", func(t *testing.T) {
		rel := filepath.Join("..", "should-not-create-"+filepath.Base(dir)+".db")
		stmts := []string{
			`attach database '` + sqlQuote(sens) + `' as s`,
			`ATTACH DATABASE ':memory:' AS s`,
			`ATTACH DATABASE 'file:` + filepath.ToSlash(sens) + `?mode=rwc' AS s`,
			`ATTACH DATABASE '` + sqlQuote(rel) + `' AS s`,
			`SELECT 1; ATTACH DATABASE '` + sqlQuote(sens) + `' AS s`,
		}
		for _, stmt := range stmts {
			before := takeFinger(t, path)
			_, err := db.ExecContext(ctx, stmt)
			assertAttachDenied(t, err)
			if _, statErr := os.Stat(sens); !os.IsNotExist(statErr) {
				t.Fatalf("文件被创建: %s", stmt)
			}
			if _, statErr := os.Stat(rel); !os.IsNotExist(statErr) {
				t.Fatalf("相对路径被创建: %s", rel)
			}
			before.assertSame(t, path)
		}
	})

	t.Run("existing-file-unchanged", func(t *testing.T) {
		if err := os.WriteFile(sens, []byte("secret-phone"), 0o600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(sens)
		if err != nil {
			t.Fatal(err)
		}
		sum := hashFile(t, sens)
		before := takeFinger(t, path)
		_, err = db.ExecContext(ctx, `ATTACH DATABASE '`+sqlQuote(sens)+`' AS s`)
		assertAttachDenied(t, err)
		again, err := os.Stat(sens)
		if err != nil {
			t.Fatal(err)
		}
		if !again.ModTime().Equal(info.ModTime()) || again.Size() != info.Size() || hashFile(t, sens) != sum {
			t.Fatal("已有的 sensitive.db 被碰到了")
		}
		before.assertSame(t, path)
		assertPathNotOpen(t, sens)
	})
}

func TestReadOnlyPoolEveryConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "analytics.db")
	w := openWritable(t, path, "DELETE")
	seedTable(t, w)
	_ = w.Close()
	removeSidecars(path)
	ctx := t.Context()
	db := openRO(t, path)
	db.SetMaxOpenConns(8)
	conns := make([]*sql.Conn, 0, 8)
	for i := 0; i < 8; i++ {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, conn)
		assertConnReadOnly(t, ctx, conn)
	}
	for _, conn := range conns {
		conn.Close()
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	assertConnReadOnly(t, ctx, conn)
}

func TestReadOnlySurvivesConnRecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "analytics.db")
	w := openWritable(t, path, "DELETE")
	seedTable(t, w)
	_ = w.Close()
	removeSidecars(path)
	ctx := t.Context()
	db := openRO(t, path)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(time.Millisecond)
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertConnReadOnly(t, ctx, conn)
	conn.Close()
	time.Sleep(20 * time.Millisecond)
	conn, err = db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	assertConnReadOnly(t, ctx, conn)
	before := takeFinger(t, path)
	if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ':memory:' AS s`); err == nil {
		t.Fatal("重建后的连接允许了 ATTACH")
	} else if !strings.Contains(strings.ToLower(err.Error()), "not authorized") {
		t.Fatalf("ATTACH: %v", err)
	}
	before.assertSame(t, path)
}

func TestSnapshotUsesEvalSymlinks(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "analytics.db")
	w := openWritable(t, real, "DELETE")
	seedTable(t, w)
	_ = w.Close()
	removeSidecars(real)
	link := filepath.Join(dir, "link.db")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatal(err)
	}
	realResolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != realResolved {
		t.Fatalf("链接 %s 和文件 %s 解析后不一致", resolved, realResolved)
	}
	viaLink := takeFinger(t, link)
	viaReal := takeFinger(t, real)
	if viaLink.resolved != viaReal.resolved || viaLink.resolved != resolved {
		t.Fatalf("解析路径 link=%s real=%s eval=%s", viaLink.resolved, viaReal.resolved, resolved)
	}
	ctx := t.Context()
	db := openRO(t, link)
	var name string
	if err := db.QueryRowContext(ctx, `SELECT name FROM t WHERE id = 1`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "a" {
		t.Fatalf("通过符号链接读到 %q", name)
	}
	viaLink.assertSame(t, link)
	if _, err := os.Stat(link + "-wal"); !os.IsNotExist(err) {
		t.Fatal("在符号链接旁边新建了 -wal")
	}
	if _, err := os.Stat(real + "-wal"); !os.IsNotExist(err) {
		t.Fatal("在真实文件旁边新建了 -wal")
	}
}

func TestSnapshotNoticesByteChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "analytics.db")
	w := openWritable(t, path, "DELETE")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	removeSidecars(path)
	before := takeFinger(t, path)
	body, err := os.ReadFile(before.resolved)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(before.resolved, append(body, 0x1), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := sqlx.Snapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := before.snap.Unchanged(after); err == nil {
		t.Fatal("主库内容变了，哈希核对却说没变")
	}
	restored := takeFinger(t, path)
	if err := os.WriteFile(path+"-wal", []byte("wal"), 0o644); err != nil {
		t.Fatal(err)
	}
	withWal, err := sqlx.Snapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.snap.Unchanged(withWal); err == nil {
		t.Fatal("新出现的 -wal 没有被算作变化")
	}
	_ = os.Remove(path + "-wal")
	noWal := takeFinger(t, path)
	if err := os.WriteFile(path+"-shm", []byte("shm-index"), 0o644); err != nil {
		t.Fatal(err)
	}
	withSHM, err := sqlx.Snapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := noWal.snap.Unchanged(withSHM); err != nil {
		t.Fatal("-shm 不在哈希范围内，却被当成了文件变化")
	}
}

func openRO(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sqlx.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func assertConnReadOnly(t *testing.T, ctx context.Context, conn *sql.Conn) {
	t.Helper()
	var q int
	if err := conn.QueryRowContext(ctx, `PRAGMA query_only`).Scan(&q); err != nil {
		t.Fatal(err)
	}
	if q != 1 {
		t.Fatalf("query_only = %d，想要 1", q)
	}
	limit, err := sqlite.Limit(conn, sqlite3.SQLITE_LIMIT_ATTACHED, -1)
	if err != nil {
		t.Fatal(err)
	}
	if limit != 0 {
		t.Fatalf("SQLITE_LIMIT_ATTACHED = %d，想要 0", limit)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO t (id, name) VALUES (9, 'no')`); err == nil {
		t.Fatal("连接接受了 INSERT")
	}
}

func assertAttachDenied(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("ATTACH 应当被拒绝")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "not authorized") {
		t.Fatalf("ATTACH: %v", err)
	}
}

func assertPathNotOpen(t *testing.T, path string) {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		if os.IsNotExist(err) {
			resolved = ""
		} else {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return
	}
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if err != nil {
			continue
		}
		if target == path || (resolved != "" && target == resolved) || strings.HasPrefix(target, path) {
			t.Fatalf("文件被打开了: %s", target)
		}
	}
}

func walAbsent(f finger) bool {
	for _, side := range f.sides {
		if side.exists {
			return false
		}
	}
	return true
}

func sqlQuote(s string) string {
	return strings.ReplaceAll(s, `'`, `''`)
}
