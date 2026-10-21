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

	if _, err := h.ConfirmExport(""); !errors.Is(err, workspace.ErrInvalidApproval) {
		t.Fatalf("ConfirmExport empty: %v", err)
	}
	assertSensitiveUntouched(t, dir)

	grant, err := h.ConfirmExport("apr-outside")
	if err != nil {
		t.Fatal(err)
	}
	assertSensitiveUntouched(t, dir)
	if grant.ApprovalID() != "apr-outside" {
		t.Fatalf("approval = %q", grant.ApprovalID())
	}

	db, err := h.OpenSensitive(ctx, grant)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_contacts`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("contacts = %d", n)
	}
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
