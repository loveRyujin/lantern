package workspace

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func TestCreateOpenLayout(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	h := mustCreate(t, ctx, root, "星禾")
	t.Cleanup(func() { _ = h.Close() })

	dir := filepath.Join(root, "workspaces", "星禾")
	if h.Dir != dir || h.ID != "星禾" || h.Root != root {
		t.Fatalf("handle = id %q root %q dir %q", h.ID, h.Root, h.Dir)
	}
	for _, name := range []string{fileKB, fileAnalytics, fileSensitive, fileRuns} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	for _, name := range []string{dirExports, dirTmp} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || !info.IsDir() {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for _, db := range []*sql.DB{h.KB, h.Analytics, h.Sensitive, h.Runs} {
		var mode string
		if err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
			t.Fatal(err)
		}
		if !strings.EqualFold(mode, "wal") {
			t.Fatalf("journal_mode = %q, want wal", mode)
		}
		var fk int
		if err := db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
			t.Fatal(err)
		}
		if fk != 1 {
			t.Fatalf("foreign_keys = %d, want 1", fk)
		}
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("schema_migrations = %d, want 1", n)
		}
	}

	users := columnNames(t, ctx, h.Analytics, "users")
	for _, forbidden := range []string{"phone", "email"} {
		if _, ok := users[forbidden]; ok {
			t.Fatalf("analytics.users has %s", forbidden)
		}
	}
	contacts := columnNames(t, ctx, h.Sensitive, "user_contacts")
	for _, want := range []string{"phone", "email"} {
		if _, ok := contacts[want]; !ok {
			t.Fatalf("sensitive.user_contacts missing %s", want)
		}
	}

	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, root, "星禾")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	var n int
	if err := reopened.KB.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("reopen applied migrations again: %d", n)
	}
}

func TestOpenMissing(t *testing.T) {
	root := t.TempDir()
	_, err := Open(t.Context(), root, "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open missing: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "workspaces")); !os.IsNotExist(statErr) {
		t.Fatalf("Open created workspaces: %v", statErr)
	}
}

func TestCreateTwice(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	h := mustCreate(t, ctx, root, "once")
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := Create(ctx, root, "once")
	if !errors.Is(err, ErrExists) {
		t.Fatalf("second Create: %v", err)
	}
}

func TestPathTraversalRejected(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	parent := filepath.Dir(root)
	outside := filepath.Join(parent, "outside-secret")
	t.Cleanup(func() { _ = os.RemoveAll(outside) })

	cases := []string{
		"..",
		".",
		"../",
		"../outside-secret",
		"foo/../../outside-secret",
		"foo/../bar",
		`..\outside-secret`,
		"a/b",
		"/etc/passwd",
		"",
		".",
		" ",
		strings.Repeat("a", 129),
	}
	for _, id := range cases {
		if _, err := Create(ctx, root, id); err == nil {
			t.Fatalf("Create(%q) succeeded", id)
		} else if id == "" || id == " " || id == strings.Repeat("a", 129) {
			if !errors.Is(err, ErrInvalidID) {
				t.Fatalf("Create(%q) = %v, want ErrInvalidID", id, err)
			}
		} else if !errors.Is(err, ErrPathEscape) {
			t.Fatalf("Create(%q) = %v, want ErrPathEscape", id, err)
		}
		if _, err := Open(ctx, root, id); err == nil {
			t.Fatalf("Open(%q) succeeded", id)
		}
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("traversal created %s: %v", outside, err)
	}
	if _, err := os.Stat(filepath.Join(root, "workspaces")); !os.IsNotExist(err) {
		t.Fatalf("rejected Create still created workspaces: %v", err)
	}
}

func TestSymlinkWorkspaceRejected(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	outside := t.TempDir()
	linkParent := filepath.Join(root, "workspaces")
	if err := os.MkdirAll(linkParent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(linkParent, "linked")); err != nil {
		t.Fatal(err)
	}
	_, err := Open(ctx, root, "linked")
	if !errors.Is(err, ErrPathEscape) {
		t.Fatalf("Open symlink = %v, want ErrPathEscape", err)
	}
}

func TestAuditAppendOnly(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	h := mustCreate(t, ctx, root, "audit")
	defer h.Close()

	if _, err := h.Runs.ExecContext(ctx,
		`INSERT INTO audit (at, actor, action, detail) VALUES (?, ?, ?, ?)`,
		"2026-10-21T00:00:00Z", "system", "create", "ok",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Runs.ExecContext(ctx, `UPDATE audit SET action = 'tamper' WHERE id = 1`); err == nil {
		t.Fatal("UPDATE audit succeeded")
	} else if !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("UPDATE audit: %v", err)
	}
	if _, err := h.Runs.ExecContext(ctx, `DELETE FROM audit WHERE id = 1`); err == nil {
		t.Fatal("DELETE audit succeeded")
	} else if !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("DELETE audit: %v", err)
	}
	var n int
	if err := h.Runs.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("audit rows = %d, want 1", n)
	}

	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, root, "audit")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.Runs.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("reopen audit rows = %d, want 1", n)
	}
	if _, err := reopened.Runs.ExecContext(ctx, `DELETE FROM audit`); err == nil {
		t.Fatal("DELETE audit succeeded after reopen")
	}
}

func TestAnalyticsAttachCannotOpenSensitive(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	h := mustCreate(t, ctx, root, "shop")
	defer h.Close()

	const phone = "13800000000"
	const email = "secret@example.com"
	if _, err := h.Sensitive.ExecContext(ctx,
		`INSERT INTO user_contacts (user_id, phone, email) VALUES (?, ?, ?)`,
		"u1", phone, email,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Analytics.ExecContext(ctx,
		`INSERT INTO users (id, display_name) VALUES (?, ?)`,
		"u1", "脱敏用户",
	); err != nil {
		t.Fatal(err)
	}

	sensitivePath := filepath.Join(h.Dir, fileSensitive)
	plain := sql.OpenDB(dsnConnector{dsn: "file::memory:?cache=shared", drv: &sqlite.Driver{}})
	defer plain.Close()
	if _, err := plain.ExecContext(ctx, `ATTACH DATABASE `+sqlLiteral(sensitivePath)+` AS leak`); err != nil {
		t.Fatalf("control ATTACH without guard: %v", err)
	}
	var gotPhone string
	if err := plain.QueryRowContext(ctx, `SELECT phone FROM leak.user_contacts WHERE user_id = 'u1'`).Scan(&gotPhone); err != nil {
		t.Fatal(err)
	}
	if gotPhone != phone {
		t.Fatalf("control read phone %q", gotPhone)
	}
	if _, err := plain.ExecContext(ctx, `DETACH DATABASE leak`); err != nil {
		t.Fatal(err)
	}

	h.Analytics.SetMaxOpenConns(4)
	attempts := []string{
		`ATTACH DATABASE ` + sqlLiteral(sensitivePath) + ` AS leak`,
		`ATTACH DATABASE ` + sqlLiteral("file:"+sensitivePath+"?mode=ro") + ` AS leak`,
		`ATTACH DATABASE ':memory:' AS leak`,
	}
	for i := 0; i < 3; i++ {
		conn, err := h.Analytics.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		limit, err := sqlite.Limit(conn, sqlite3.SQLITE_LIMIT_ATTACHED, -1)
		if err != nil {
			conn.Close()
			t.Fatal(err)
		}
		if limit != 0 {
			conn.Close()
			t.Fatalf("SQLITE_LIMIT_ATTACHED = %d, want 0", limit)
		}
		for _, stmt := range attempts {
			if _, err := conn.ExecContext(ctx, stmt); err == nil {
				conn.Close()
				t.Fatalf("ATTACH succeeded: %s", stmt)
			} else if !strings.Contains(strings.ToLower(err.Error()), "not authorized") {
				conn.Close()
				t.Fatalf("ATTACH %s: %v", stmt, err)
			}
		}
		var leaked string
		err = conn.QueryRowContext(ctx, `SELECT phone FROM leak.user_contacts`).Scan(&leaked)
		conn.Close()
		if err == nil {
			t.Fatalf("analytics read sensitive phone %q", leaked)
		}
	}

	var still string
	if err := h.Sensitive.QueryRowContext(ctx, `SELECT email FROM user_contacts WHERE user_id = 'u1'`).Scan(&still); err != nil {
		t.Fatal(err)
	}
	if still != email {
		t.Fatalf("sensitive email = %q", still)
	}
}

func TestVectorsNewDroppedOnOpen(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	h := mustCreate(t, ctx, root, "kb")
	if _, err := h.KB.ExecContext(ctx, `CREATE TABLE vectors_new (chunk_id TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, root, "kb")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var n int
	if err := reopened.KB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'vectors_new'`,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("vectors_new survived reopen")
	}
}

func TestIndexMetaSingleRow(t *testing.T) {
	ctx := t.Context()
	h := mustCreate(t, ctx, t.TempDir(), "idx")
	defer h.Close()
	if _, err := h.KB.ExecContext(ctx,
		`INSERT INTO index_meta (id, embedder_id, dim, normalize, created_at) VALUES (1, 'mock', 3, 1, 't')`,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := h.KB.ExecContext(ctx,
		`INSERT INTO index_meta (id, embedder_id, dim, normalize, created_at) VALUES (2, 'other', 3, 1, 't')`,
	); err == nil {
		t.Fatal("second index_meta row succeeded")
	}
}

func TestDataRoot(t *testing.T) {
	got, err := dataRoot("darwin", "/Users/me", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join("/Users/me", "Library", "Application Support", "Lantern") {
		t.Fatalf("darwin = %s", got)
	}
	got, err = dataRoot("linux", "/home/me", "/xdg", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join("/xdg", "lantern") {
		t.Fatalf("linux xdg = %s", got)
	}
	got, err = dataRoot("linux", "/home/me", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join("/home/me", ".local", "share", "lantern") {
		t.Fatalf("linux default = %s", got)
	}
	got, err = dataRoot("windows", `C:\Users\me`, "", `D:\AppData`)
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(`D:\AppData`, "Lantern") {
		t.Fatalf("windows = %s", got)
	}
	if _, err := dataRoot("linux", "", "", ""); err == nil {
		t.Fatal("linux without home succeeded")
	}
}

func mustCreate(t *testing.T, ctx context.Context, root, id string) *Handle {
	t.Helper()
	h, err := Create(ctx, root, id)
	if err != nil {
		t.Fatalf("Create(%q): %v", id, err)
	}
	return h
}

func columnNames(t *testing.T, ctx context.Context, db *sql.DB, table string) map[string]struct{} {
	t.Helper()
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	names := map[string]struct{}{}
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		names[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

func sqlLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
