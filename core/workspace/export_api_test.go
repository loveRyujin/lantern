package workspace_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/approval"
	"github.com/loveRyujin/lantern/core/errcode"
	"github.com/loveRyujin/lantern/core/workspace"
)

// TestExportAPIsRefuseWithoutGrant 从包外调用每一个会返回 *sql.DB 的导出方法。
// 零值凭证必须失败，并且 sensitive.db 仍然不存在、也没有被进程打开。
func TestExportAPIsRefuseWithoutGrant(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	h, err := workspace.Create(ctx, root, "export-api")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })

	dir := filepath.Join(root, "workspaces", "export-api")
	assertSensitiveUntouched(t, dir)

	typ := reflect.TypeOf(h)
	dbType := reflect.TypeOf((*sql.DB)(nil))
	grantType := reflect.TypeOf(workspace.ExportGrant{})
	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
	errType := reflect.TypeOf((*error)(nil)).Elem()
	found := 0
	for i := 0; i < typ.NumMethod(); i++ {
		m := typ.Method(i)
		mt := m.Type
		dbOuts := []int{}
		errOut := -1
		for o := 0; o < mt.NumOut(); o++ {
			switch mt.Out(o) {
			case dbType:
				dbOuts = append(dbOuts, o)
			case errType:
				errOut = o
			}
		}
		if len(dbOuts) == 0 {
			continue
		}
		found++
		if errOut < 0 {
			t.Fatalf("%s returns *sql.DB without an error", m.Name)
		}
		hasGrant := false
		args := make([]reflect.Value, mt.NumIn())
		args[0] = reflect.ValueOf(h)
		for a := 1; a < mt.NumIn(); a++ {
			in := mt.In(a)
			switch in {
			case grantType:
				hasGrant = true
				args[a] = reflect.Zero(grantType)
			case ctxType:
				args[a] = reflect.ValueOf(ctx)
			default:
				args[a] = reflect.Zero(in)
			}
		}
		if !hasGrant {
			t.Fatalf("%s returns *sql.DB without workspace.ExportGrant", m.Name)
		}
		out := m.Func.Call(args)
		for _, o := range dbOuts {
			if !out[o].IsNil() {
				t.Fatalf("%s returned a connection without a grant", m.Name)
			}
		}
		got, _ := out[errOut].Interface().(error)
		if !errors.Is(got, workspace.ErrExportUnconfirmed) {
			t.Fatalf("%s err = %v, want ErrExportUnconfirmed", m.Name, got)
		}
	}
	if found == 0 {
		t.Fatal("no export API returns *sql.DB")
	}
	assertSensitiveUntouched(t, dir)

	if _, err := h.ConfirmExport(ctx, "", "users"); !errors.Is(err, workspace.ErrInvalidApproval) || !errors.Is(err, workspace.ErrExportUnconfirmed) {
		t.Fatalf("ConfirmExport empty: %v", err)
	}
	assertSensitiveUntouched(t, dir)

	// 任意字符串不是审批记录，不能签发凭证，也不能打开敏感库。
	grant, err := h.ConfirmExport(ctx, "apr-outside", "users")
	if !errors.Is(err, workspace.ErrExportUnconfirmed) {
		t.Fatalf("arbitrary token confirmed: %v", err)
	}
	assertExportDenied(t, ctx, h, dir, grant, err)
}

func TestHandleDoesNotExportSensitiveField(t *testing.T) {
	h, err := workspace.Create(t.Context(), t.TempDir(), "fields")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })

	v := reflect.ValueOf(h).Elem()
	dbType := reflect.TypeOf((*sql.DB)(nil))
	seen := map[string]bool{}
	for i := 0; i < v.NumField(); i++ {
		f := v.Type().Field(i)
		if f.PkgPath != "" {
			continue
		}
		if strings.Contains(strings.ToLower(f.Name), "sensitive") {
			t.Fatalf("exported field %s exposes sensitive state", f.Name)
		}
		if f.Type == dbType {
			seen[f.Name] = true
		}
	}
	for _, name := range []string{"KB", "Analytics", "Runs"} {
		if !seen[name] {
			t.Fatalf("missing exported field %s", name)
		}
		delete(seen, name)
	}
	for name := range seen {
		t.Fatalf("unexpected exported *sql.DB field %s", name)
	}
}

func TestOpenSensitiveForgedToken(t *testing.T) {
	ctx := t.Context()
	h, dir := newExportWorkspace(t, ctx, "forged")
	grant, err := h.ConfirmExport(ctx, "forged-token", "users")
	assertExportDenied(t, ctx, h, dir, grant, err)
}

func TestOpenSensitiveUnapproved(t *testing.T) {
	ctx := t.Context()
	h, dir := newExportWorkspace(t, ctx, "pending")
	insertApproval(t, ctx, h, "apr-pending", h.ID, "export_data", "users", "", time.Now().Add(time.Hour))
	grant, err := h.ConfirmExport(ctx, "apr-pending", "users")
	assertExportDenied(t, ctx, h, dir, grant, err)

	insertApproval(t, ctx, h, "apr-defer", h.ID, "export_data", "users", string(approval.ActionDefer), time.Now().Add(time.Hour))
	grant, err = h.ConfirmExport(ctx, "apr-defer", "users")
	assertExportDenied(t, ctx, h, dir, grant, err)
}

func TestOpenSensitiveRejected(t *testing.T) {
	ctx := t.Context()
	h, dir := newExportWorkspace(t, ctx, "rejected")
	insertApproval(t, ctx, h, "apr-reject", h.ID, "export_data", "users", string(approval.ActionReject), time.Now().Add(time.Hour))
	grant, err := h.ConfirmExport(ctx, "apr-reject", "users")
	assertExportDenied(t, ctx, h, dir, grant, err)
}

func TestOpenSensitiveCrossWorkspace(t *testing.T) {
	ctx := t.Context()
	h, dir := newExportWorkspace(t, ctx, "here")
	insertApproval(t, ctx, h, "apr-there", "there", "export_data", "users", string(approval.ActionApprove), time.Now().Add(time.Hour))
	grant, err := h.ConfirmExport(ctx, "apr-there", "users")
	assertExportDenied(t, ctx, h, dir, grant, err)
}

func TestOpenSensitiveWrongObject(t *testing.T) {
	ctx := t.Context()
	h, dir := newExportWorkspace(t, ctx, "object")
	insertApproval(t, ctx, h, "apr-object", h.ID, "export_data", "orders", string(approval.ActionApprove), time.Now().Add(time.Hour))
	grant, err := h.ConfirmExport(ctx, "apr-object", "users")
	assertExportDenied(t, ctx, h, dir, grant, err)

	// 对象不一致不能把确认单用掉，正确的对象仍然可以打开一次。
	grant, err = h.ConfirmExport(ctx, "apr-object", "orders")
	if err != nil {
		t.Fatal(err)
	}
	db, err := h.OpenSensitive(ctx, grant)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
}

func TestOpenSensitiveExpired(t *testing.T) {
	ctx := t.Context()
	h, dir := newExportWorkspace(t, ctx, "expired")
	insertApproval(t, ctx, h, "apr-expired", h.ID, "export_data", "users", string(approval.ActionApprove), time.Now().Add(-time.Hour))
	grant, err := h.ConfirmExport(ctx, "apr-expired", "users")
	assertExportDenied(t, ctx, h, dir, grant, err)
}

func TestOpenSensitiveNotExportAction(t *testing.T) {
	ctx := t.Context()
	h, dir := newExportWorkspace(t, ctx, "write")
	insertApproval(t, ctx, h, "apr-write", h.ID, "write_action", "users", string(approval.ActionApprove), time.Now().Add(time.Hour))
	grant, err := h.ConfirmExport(ctx, "apr-write", "users")
	assertExportDenied(t, ctx, h, dir, grant, err)
}

func TestOpenSensitiveReplay(t *testing.T) {
	ctx := t.Context()
	h, _ := newExportWorkspace(t, ctx, "replay")
	insertApproval(t, ctx, h, "apr-replay", h.ID, "export_data", "users", string(approval.ActionApprove), time.Now().Add(time.Hour))
	grant, err := h.ConfirmExport(ctx, "apr-replay", "users")
	if err != nil {
		t.Fatal(err)
	}
	db, err := h.OpenSensitive(ctx, grant)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	again, err := h.OpenSensitive(ctx, grant)
	if again != nil || !errors.Is(err, workspace.ErrExportUnconfirmed) {
		t.Fatalf("replay grant: db=%v err=%v", again, err)
	}
	if code, ok := errcode.CodeOf(err); !ok || code != errcode.ExportUnconfirmed {
		t.Fatalf("replay grant code = %s, ok %v", code, ok)
	}
	reissued, err := h.ConfirmExport(ctx, "apr-replay", "users")
	if !errors.Is(err, workspace.ErrExportUnconfirmed) {
		t.Fatalf("replay approval: %v", err)
	}
	if code, ok := errcode.CodeOf(err); !ok || code != errcode.ExportUnconfirmed {
		t.Fatalf("replay approval code = %s, ok %v", code, ok)
	}
	opened, err := h.OpenSensitive(ctx, reissued)
	if opened != nil || !errors.Is(err, workspace.ErrExportUnconfirmed) {
		t.Fatalf("replay open: db=%v err=%v", opened, err)
	}
}

func newExportWorkspace(t *testing.T, ctx context.Context, id string) (*workspace.Handle, string) {
	t.Helper()
	root := t.TempDir()
	h, err := workspace.Create(ctx, root, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h, filepath.Join(root, "workspaces", id)
}

func insertApproval(t *testing.T, ctx context.Context, h *workspace.Handle, id, workspaceID, action, object, decision string, expires time.Time) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	runID := "run-" + id
	if _, err := h.Runs.ExecContext(ctx, `
		INSERT INTO runs (id, state, version, context, created_at, updated_at)
		VALUES (?, 'awaiting_approval', 0, '', ?, ?)`, runID, now, now); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	var decided any
	if decision != "" {
		decided = now
	}
	if _, err := h.Runs.ExecContext(ctx, `
		INSERT INTO approvals (
			id, run_id, step_id, params_hash, params_json, decision, created_at, decided_at,
			workspace_id, action, export_object, expires_at
		) VALUES (?, ?, 'step', ?, '{}', ?, ?, ?, ?, ?, ?, ?)`,
		id, runID, strings.Repeat("ab", 32), decision, now, decided,
		workspaceID, action, object, expires.UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("insert approval: %v", err)
	}
}

func assertExportDenied(t *testing.T, ctx context.Context, h *workspace.Handle, dir string, grant workspace.ExportGrant, err error) {
	t.Helper()
	if !errors.Is(err, workspace.ErrExportUnconfirmed) {
		t.Fatalf("confirm: %v", err)
	}
	if code, ok := errcode.CodeOf(err); !ok || code != errcode.ExportUnconfirmed {
		t.Fatalf("confirm code = %s, ok %v", code, ok)
	}
	if grant.ApprovalID() != "" {
		t.Fatalf("denied confirm still returned approval %q", grant.ApprovalID())
	}
	db, openErr := h.OpenSensitive(ctx, grant)
	if db != nil {
		_ = db.Close()
		t.Fatal("OpenSensitive returned a connection")
	}
	if !errors.Is(openErr, workspace.ErrExportUnconfirmed) {
		t.Fatalf("open: %v", openErr)
	}
	if code, ok := errcode.CodeOf(openErr); !ok || code != errcode.ExportUnconfirmed {
		t.Fatalf("open code = %s, ok %v", code, ok)
	}
	assertSensitiveUntouched(t, dir)
}

func assertSensitiveUntouched(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"sensitive.db", "sensitive.db-wal", "sensitive.db-shm"} {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s exists: %v", name, err)
		}
	}
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return
	}
	needle := filepath.Join(dir, "sensitive.db")
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if err != nil {
			continue
		}
		if target == needle || strings.HasPrefix(target, needle) {
			t.Fatalf("sensitive.db is open: %s", target)
		}
	}
}
