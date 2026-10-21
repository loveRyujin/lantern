package workspace

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/loveRyujin/lantern/core/errcode"
)

func TestSensitiveStaysClosedUntilConfirmed(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	h := mustCreate(t, ctx, root, "closed")
	t.Cleanup(func() { _ = h.Close() })
	assertSensitiveAbsent(t, h.Dir)

	if _, err := h.ConfirmExport(ctx, "", exportObjectContacts); !errors.Is(err, ErrInvalidApproval) || !errors.Is(err, ErrExportUnconfirmed) {
		t.Fatalf("empty approval: %v", err)
	}
	if _, err := h.ConfirmExport(ctx, "  ", exportObjectContacts); !errors.Is(err, ErrInvalidApproval) {
		t.Fatalf("blank approval: %v", err)
	}
	if _, err := h.ConfirmExport(ctx, "不存在", exportObjectContacts); !errors.Is(err, ErrExportUnconfirmed) {
		t.Fatalf("missing approval: %v", err)
	} else if code, ok := errcode.CodeOf(err); !ok || code != errcode.ExportUnconfirmed {
		t.Fatalf("missing approval code = %s, ok %v", code, ok)
	}
	if _, err := h.OpenSensitive(ctx, ExportGrant{}); !errors.Is(err, ErrExportUnconfirmed) {
		t.Fatalf("zero grant: %v", err)
	}
	if _, err := h.OpenSensitive(ctx, ExportGrant{h: h, id: 1, approvalID: "guess"}); !errors.Is(err, ErrExportUnconfirmed) {
		t.Fatalf("forged grant: %v", err)
	}
	assertSensitiveAbsent(t, h.Dir)

	if _, err := h.ConfirmExport(ctx, "确认-1", exportObjectContacts); !errors.Is(err, ErrExportUnconfirmed) {
		t.Fatalf("unrecorded approval: %v", err)
	}
	assertSensitiveAbsent(t, h.Dir)
	mustApproveExport(t, ctx, h, "确认-1", exportObjectContacts)
	grant, err := h.ConfirmExport(ctx, "确认-1", exportObjectContacts)
	if err != nil {
		t.Fatal(err)
	}
	if grant.ApprovalID() != "确认-1" {
		t.Fatalf("approval id = %q", grant.ApprovalID())
	}
	assertSensitiveAbsent(t, h.Dir)

	db, err := h.OpenSensitive(ctx, grant)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	contacts := columnNames(t, ctx, db, "user_contacts")
	for _, want := range []string{"phone", "email"} {
		if _, ok := contacts[want]; !ok {
			t.Fatalf("user_contacts missing %s", want)
		}
	}
	var mode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Fatalf("journal_mode = %q", mode)
	}
	assertAttachDenied(t, ctx, db)

	if _, err := h.OpenSensitive(ctx, grant); !errors.Is(err, ErrExportUnconfirmed) {
		t.Fatalf("second open: %v", err)
	}
	if _, err := h.ConfirmExport(ctx, "确认-1", exportObjectContacts); !errors.Is(err, ErrExportUnconfirmed) {
		t.Fatalf("second confirm: %v", err)
	}
}

func TestOpenDoesNotTouchSensitiveFile(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	h := mustCreate(t, ctx, root, "sentinel")
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	// 空文件一旦被 SQLite 打开就会写上库头。内容保持为空，说明 Open 没有打开它。
	path := filepath.Join(root, "workspaces", "sentinel", fileSensitive)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, root, "sentinel")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	assertSensitiveFileEmpty(t, path)
	assertNoSensitiveFD(t, reopened.Dir)
	if _, err := reopened.OpenSensitive(ctx, ExportGrant{}); !errors.Is(err, ErrExportUnconfirmed) {
		t.Fatalf("open after sentinel: %v", err)
	}
	assertSensitiveFileEmpty(t, path)
}

func assertSensitiveFileEmpty(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Fatalf("%s size = %d, sqlite opened the empty file", path, info.Size())
	}
}

func TestGrantIsBoundToIssuingWorkspace(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	a := mustCreate(t, ctx, root, "a")
	b := mustCreate(t, ctx, root, "b")
	t.Cleanup(func() { _ = a.Close() })
	t.Cleanup(func() { _ = b.Close() })

	mustApproveExport(t, ctx, a, "apr-a", exportObjectContacts)
	grant, err := a.ConfirmExport(ctx, "apr-a", exportObjectContacts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.OpenSensitive(ctx, grant); !errors.Is(err, ErrExportUnconfirmed) {
		t.Fatalf("cross workspace: %v", err)
	}
	assertSensitiveAbsent(t, b.Dir)

	swapped := grant
	swapped.approvalID = "apr-other"
	if _, err := a.OpenSensitive(ctx, swapped); !errors.Is(err, ErrExportUnconfirmed) {
		t.Fatalf("swapped approval: %v", err)
	}
	assertSensitiveAbsent(t, a.Dir)

	db, err := a.OpenSensitive(ctx, grant)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
}

func TestCanceledOpenDoesNotSpendGrant(t *testing.T) {
	ctx := t.Context()
	h := mustCreate(t, ctx, t.TempDir(), "cancel")
	t.Cleanup(func() { _ = h.Close() })
	mustApproveExport(t, ctx, h, "apr-cancel", exportObjectContacts)
	grant, err := h.ConfirmExport(ctx, "apr-cancel", exportObjectContacts)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := h.OpenSensitive(canceled, grant); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled open: %v", err)
	}
	assertSensitiveAbsent(t, h.Dir)

	db, err := h.OpenSensitive(ctx, grant)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
}

func TestCloseRevokesGrant(t *testing.T) {
	ctx := t.Context()
	h := mustCreate(t, ctx, t.TempDir(), "revoke")
	mustApproveExport(t, ctx, h, "apr-revoke", exportObjectContacts)
	grant, err := h.ConfirmExport(ctx, "apr-revoke", exportObjectContacts)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.OpenSensitive(ctx, grant); !errors.Is(err, ErrExportUnconfirmed) {
		t.Fatalf("open after close: %v", err)
	}
	if _, err := h.ConfirmExport(ctx, "apr-new", exportObjectContacts); !errors.Is(err, ErrExportUnconfirmed) {
		t.Fatalf("confirm after close: %v", err)
	}
	assertSensitiveAbsent(t, h.Dir)
}

func TestCloseAlsoClosesSensitive(t *testing.T) {
	ctx := t.Context()
	h := mustCreate(t, ctx, t.TempDir(), "shutdown")
	db := mustOpenSensitive(t, ctx, h, "apr-shutdown")
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.PingContext(ctx); err == nil {
		t.Fatal("sensitive connection still open after Handle.Close")
	}
}

func TestSensitiveMigrationRunsOnce(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	h := mustCreate(t, ctx, root, "mig")
	db := mustOpenSensitive(t, ctx, h, "apr-mig-1")
	if _, err := db.ExecContext(ctx,
		`INSERT INTO user_contacts (user_id, phone, email) VALUES ('u', '1', 'a@b.c')`,
	); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(ctx, root, "mig")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	assertNoSensitiveFD(t, reopened.Dir)

	again := mustOpenSensitive(t, ctx, reopened, "apr-mig-2")
	var n int
	if err := again.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("schema_migrations = %d, want 1", n)
	}
	var phone string
	if err := again.QueryRowContext(ctx, `SELECT phone FROM user_contacts WHERE user_id = 'u'`).Scan(&phone); err != nil {
		t.Fatal(err)
	}
	if phone != "1" {
		t.Fatalf("phone = %q", phone)
	}
}

func TestOpenSensitiveDoesNotAddFDWhenRefused(t *testing.T) {
	ctx := t.Context()
	h := mustCreate(t, ctx, t.TempDir(), "fds")
	t.Cleanup(func() { _ = h.Close() })
	db := mustOpenSensitive(t, ctx, h, "apr-fd")
	if err := db.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	before := countSensitiveFDs(t, h.Dir)
	if before < 0 {
		t.Skip("fd scan unavailable")
	}
	if before == 0 {
		t.Fatal("confirmed connection does not hold sensitive.db open")
	}
	if _, err := h.OpenSensitive(ctx, ExportGrant{}); !errors.Is(err, ErrExportUnconfirmed) {
		t.Fatal(err)
	}
	if after := countSensitiveFDs(t, h.Dir); after != before {
		t.Fatalf("sensitive fds = %d, want %d", after, before)
	}
}

func TestOneGrantOpensOnceUnderRace(t *testing.T) {
	ctx := t.Context()
	h := mustCreate(t, ctx, t.TempDir(), "race")
	t.Cleanup(func() { _ = h.Close() })
	mustApproveExport(t, ctx, h, "apr-race", exportObjectContacts)
	grant, err := h.ConfirmExport(ctx, "apr-race", exportObjectContacts)
	if err != nil {
		t.Fatal(err)
	}

	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	var opened []*sql.DB
	var unexpected []error
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			db, err := h.OpenSensitive(ctx, grant)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if !errors.Is(err, ErrExportUnconfirmed) {
					unexpected = append(unexpected, err)
				}
				return
			}
			opened = append(opened, db)
		}()
	}
	wg.Wait()
	for _, db := range opened {
		_ = db.Close()
	}
	if len(unexpected) > 0 {
		t.Fatalf("unexpected errors: %v", unexpected)
	}
	if len(opened) != 1 {
		t.Fatalf("opened %d connections, want 1", len(opened))
	}
}

func TestNilHandleRejectsExport(t *testing.T) {
	var h *Handle
	if _, err := h.ConfirmExport(t.Context(), "apr", exportObjectContacts); !errors.Is(err, ErrExportUnconfirmed) {
		t.Fatalf("nil confirm: %v", err)
	}
	if _, err := h.OpenSensitive(t.Context(), ExportGrant{}); !errors.Is(err, ErrExportUnconfirmed) {
		t.Fatalf("nil open: %v", err)
	}
}
