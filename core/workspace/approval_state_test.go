package workspace

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/approval"
	"github.com/loveRyujin/lantern/core/errcode"
	_ "modernc.org/sqlite"
)

func TestApprovalStateBackfill(t *testing.T) {
	db := openRawRunsDB(t)
	applyRunsSQL(t, db, "001_init.sql", "002_export_approval.sql")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`
		INSERT INTO runs (id, state, version, context, created_at, updated_at)
		VALUES ('run', 'running', 0, '', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	rows := []struct {
		id       string
		decision string
		consumed any
		want     string
	}{
		{id: "approved", decision: "approve", want: "approved"},
		{id: "approved-empty", decision: "approve", consumed: "", want: "approved"},
		{id: "consumed", decision: "approve", consumed: now, want: "consumed"},
		{id: "rejected", decision: "reject", want: "rejected"},
		{id: "pending", decision: "", want: "pending"},
		{id: "deferred", decision: "defer", want: "pending"},
		{id: "past", decision: "approve", want: "approved"},
	}
	for _, row := range rows {
		expires := now
		if row.id == "past" {
			expires = past
		}
		if _, err := db.Exec(`
			INSERT INTO approvals (
				id, run_id, step_id, params_hash, params_json, decision, created_at,
				workspace_id, action, export_object, expires_at, consumed_at
			) VALUES (?, 'run', 'step', ?, '{}', ?, ?, '', '', '', ?, ?)`,
			row.id, strings.Repeat("ab", 32), row.decision, now, expires, row.consumed); err != nil {
			t.Fatalf("插入 %s: %v", row.id, err)
		}
	}
	applyRunsSQL(t, db, "003_approval_state.sql")
	for _, row := range rows {
		var state string
		var version int64
		if err := db.QueryRow(`SELECT state, version FROM approvals WHERE id = ?`, row.id).Scan(&state, &version); err != nil {
			t.Fatal(err)
		}
		if state != row.want || version != 0 {
			t.Fatalf("%s 回填 = %s 版本 %d，想要 %s 版本 0", row.id, state, version, row.want)
		}
	}
}

func TestCreatedWorkspaceHasApprovalState(t *testing.T) {
	h := mustCreate(t, t.Context(), t.TempDir(), "cols")
	t.Cleanup(func() { _ = h.Close() })
	seen := map[string]bool{}
	rows, err := h.Runs.Query(`PRAGMA table_info(approvals)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		seen[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !seen["state"] || !seen["version"] {
		t.Fatalf("列 = %v", seen)
	}
}

func TestConfirmExportMismatchDoesNotOpenOrConsume(t *testing.T) {
	ctx := t.Context()
	h := mustCreate(t, ctx, t.TempDir(), "mismatch")
	t.Cleanup(func() { _ = h.Close() })
	mustApproveExport(t, ctx, h, "apr", exportObjectContacts)
	good := exportCC("apr")
	cases := []approval.ConsumeContext{
		{RunID: "other", StepID: good.StepID, ArgsHash: good.ArgsHash},
		{RunID: good.RunID, StepID: "other", ArgsHash: good.ArgsHash},
		{RunID: good.RunID, StepID: good.StepID, ArgsHash: strings.ToUpper(good.ArgsHash)},
		{RunID: good.RunID, StepID: good.StepID, ArgsHash: ""},
		{},
	}
	for _, cc := range cases {
		_, err := h.ConfirmExport(ctx, "apr", cc, exportObjectContacts)
		if !errors.Is(err, approval.ErrMismatch) || errors.Is(err, ErrExportUnconfirmed) || errors.Is(err, approval.ErrSnapshotMismatch) {
			t.Fatalf("核对 %q = %v", cc, err)
		}
		if code, ok := errcode.CodeOf(err); !ok || code != errcode.ApprovalMismatch {
			t.Fatalf("错误码 = %s ok=%v", code, ok)
		}
		state, version := approvalRow(t, ctx, h, "apr")
		if state != string(approval.StateApproved) || version != 0 {
			t.Fatalf("核对失败后 state=%s version=%d", state, version)
		}
		assertSensitiveAbsent(t, h.Dir)
	}
	grant, err := h.ConfirmExport(ctx, "apr", good, exportObjectContacts)
	if err != nil {
		t.Fatal(err)
	}
	state, version := approvalRow(t, ctx, h, "apr")
	if state != string(approval.StateConsumed) || version != 1 || grant.ApprovalID() != "apr" {
		t.Fatalf("正确核对后 state=%s version=%d", state, version)
	}
}

func TestSecondConfirmDoesNotOpenSensitive(t *testing.T) {
	ctx := t.Context()
	h := mustCreate(t, ctx, t.TempDir(), "once")
	t.Cleanup(func() { _ = h.Close() })
	mustApproveExport(t, ctx, h, "apr", exportObjectContacts)
	before := countSensitiveFDs(t, h.Dir)
	grant, err := h.ConfirmExport(ctx, "apr", exportCC("apr"), exportObjectContacts)
	if err != nil {
		t.Fatal(err)
	}
	if got := countSensitiveFDs(t, h.Dir); before >= 0 && got != before {
		t.Fatalf("确认本身打开了敏感库 %d → %d", before, got)
	}
	if _, err := h.ConfirmExport(ctx, "apr", exportCC("apr"), exportObjectContacts); !errors.Is(err, ErrExportUnconfirmed) || errors.Is(err, approval.ErrMismatch) {
		t.Fatalf("第二次确认 = %v", err)
	}
	if got := countSensitiveFDs(t, h.Dir); before >= 0 && got != before {
		t.Fatalf("第二次确认打开了敏感库 %d → %d", before, got)
	}
	db, err := h.OpenSensitive(ctx, grant)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	opened := countSensitiveFDs(t, h.Dir)
	if before >= 0 && opened <= before {
		t.Fatalf("打开后句柄没有增加 %d → %d", before, opened)
	}
	if _, err := h.ConfirmExport(ctx, "apr", exportCC("apr"), exportObjectContacts); !errors.Is(err, ErrExportUnconfirmed) {
		t.Fatalf("打开后再确认 = %v", err)
	}
	if got := countSensitiveFDs(t, h.Dir); before >= 0 && got != opened {
		t.Fatalf("再次确认让敏感库句柄从 %d 变成 %d", opened, got)
	}
}

func TestConfirmExportSignature(t *testing.T) {
	ctx := t.Context()
	h := mustCreate(t, ctx, t.TempDir(), "sig")
	t.Cleanup(func() { _ = h.Close() })
	_, err := confirmExportSignature(h, ctx, "missing", approval.ConsumeContext{}, exportObjectContacts)
	if !errors.Is(err, ErrExportUnconfirmed) {
		t.Fatalf("签名调用 = %v", err)
	}
}

func confirmExportSignature(h *Handle, ctx context.Context, id string, cc approval.ConsumeContext, object string) (ExportGrant, error) {
	return h.ConfirmExport(ctx, id, cc, object)
}

func TestEightConsumersOpenSensitiveOnce(t *testing.T) {
	ctx := t.Context()
	h := mustCreate(t, ctx, t.TempDir(), "eight")
	t.Cleanup(func() { _ = h.Close() })
	mustApproveExport(t, ctx, h, "apr", exportObjectContacts)
	const n = 8
	var wg sync.WaitGroup
	grants := make([]ExportGrant, n)
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			grants[i], errs[i] = h.ConfirmExport(ctx, "apr", exportCC("apr"), exportObjectContacts)
		}(i)
	}
	wg.Wait()
	wins := 0
	var grant ExportGrant
	for i, err := range errs {
		if err == nil {
			wins++
			grant = grants[i]
			continue
		}
		if errors.Is(err, approval.ErrMismatch) || !errors.Is(err, ErrExportUnconfirmed) {
			t.Fatalf("失败方 = %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("成功 %d 次", wins)
	}
	before := countSensitiveFDs(t, h.Dir)
	db, err := h.OpenSensitive(ctx, grant)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if got := countSensitiveFDs(t, h.Dir); before >= 0 && got <= before {
		t.Fatalf("唯一的成功没有打开敏感库 %d → %d", before, got)
	}
	state, version := approvalRow(t, ctx, h, "apr")
	if state != string(approval.StateConsumed) || version != 1 {
		t.Fatalf("并发消费后 state=%s version=%d", state, version)
	}
}

func TestForgedApprovalIDDoesNotInject(t *testing.T) {
	ctx := t.Context()
	h := mustCreate(t, ctx, t.TempDir(), "inject")
	t.Cleanup(func() { _ = h.Close() })
	mustApproveExport(t, ctx, h, "legit", exportObjectContacts)
	for _, id := range []string{strings.Repeat("a", 257), "x'OR'1'='1", "';DROP"} {
		_, err := h.ConfirmExport(ctx, id, exportCC("legit"), exportObjectContacts)
		if err == nil {
			t.Fatalf("%q 被接受", id)
		}
	}
	if _, err := h.ConfirmExport(ctx, "' OR 1=1--", exportCC("legit"), exportObjectContacts); !errors.Is(err, ErrInvalidApproval) {
		t.Fatalf("带空格的伪造标识 = %v", err)
	}
	var n int
	if err := h.Runs.QueryRowContext(ctx, `SELECT COUNT(*) FROM approvals`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("确认单条数 = %d", n)
	}
	if _, err := h.Runs.ExecContext(ctx, `SELECT COUNT(*) FROM approvals`); err != nil {
		t.Fatal(err)
	}
	grant, err := h.ConfirmExport(ctx, "legit", exportCC("legit"), exportObjectContacts)
	if err != nil {
		t.Fatalf("合法确认单被注入弄坏: %v", err)
	}
	if grant.ApprovalID() != "legit" {
		t.Fatalf("凭证 = %s", grant.ApprovalID())
	}
}

func TestConsumedApprovalRejectedAfterReopen(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	h := mustCreate(t, ctx, root, "reopen")
	mustApproveExport(t, ctx, h, "apr", exportObjectContacts)
	grant, err := h.ConfirmExport(ctx, "apr", exportCC("apr"), exportObjectContacts)
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
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, root, "reopen")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if _, err := reopened.ConfirmExport(ctx, "apr", exportCC("apr"), exportObjectContacts); !errors.Is(err, ErrExportUnconfirmed) {
		t.Fatalf("重开后重放 = %v", err)
	}
	state, version := approvalRow(t, ctx, reopened, "apr")
	if state != string(approval.StateConsumed) || version != 1 {
		t.Fatalf("重开后 state=%s version=%d", state, version)
	}
	assertNoSensitiveFD(t, reopened.Dir)
}

func TestOtherWorkspaceCannotConsume(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	a := mustCreate(t, ctx, root, "a")
	b := mustCreate(t, ctx, root, "b")
	t.Cleanup(func() { _ = a.Close() })
	t.Cleanup(func() { _ = b.Close() })
	mustApproveExport(t, ctx, a, "apr", exportObjectContacts)
	if _, err := b.ConfirmExport(ctx, "apr", exportCC("apr"), exportObjectContacts); !errors.Is(err, ErrExportUnconfirmed) {
		t.Fatalf("别的工作区 = %v", err)
	}
	assertSensitiveAbsent(t, b.Dir)
	state, version := approvalRow(t, ctx, a, "apr")
	if state != string(approval.StateApproved) || version != 0 {
		t.Fatalf("原确认单 state=%s version=%d", state, version)
	}
	if _, err := a.ConfirmExport(ctx, "apr", exportCC("apr"), exportObjectContacts); err != nil {
		t.Fatal(err)
	}
}

func TestSQLDecisionBypassIsCorrupt(t *testing.T) {
	ctx := t.Context()
	h := mustCreate(t, ctx, t.TempDir(), "bypass")
	t.Cleanup(func() { _ = h.Close() })
	insertExportApproval(t, ctx, h, exportApproval{
		id:          "apr",
		workspaceID: h.ID,
		action:      actionExportData,
		object:      exportObjectContacts,
		decision:    "",
		expiresAt:   time.Now().Add(time.Hour),
	})
	if _, err := h.Runs.ExecContext(ctx, `UPDATE approvals SET decision = 'approve' WHERE id = ?`, "apr"); err != nil {
		t.Fatal(err)
	}
	_, err := h.ConfirmExport(ctx, "apr", exportCC("apr"), exportObjectContacts)
	if !errors.Is(err, approval.ErrCorrupt) || !errors.Is(err, ErrExportUnconfirmed) || errors.Is(err, approval.ErrMismatch) {
		t.Fatalf("绕过写决定 = %v", err)
	}
	state, version := approvalRow(t, ctx, h, "apr")
	if state != string(approval.StatePending) || version != 0 {
		t.Fatalf("绕过之后 state=%s version=%d", state, version)
	}
	assertSensitiveAbsent(t, h.Dir)
	var audits int
	if err := h.Runs.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit WHERE action = 'corrupt'`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Fatalf("损坏审计 = %d", audits)
	}
}

func approvalRow(t *testing.T, ctx context.Context, h *Handle, id string) (string, int64) {
	t.Helper()
	var state string
	var version int64
	if err := h.Runs.QueryRowContext(ctx, `SELECT state, version FROM approvals WHERE id = ?`, id).Scan(&state, &version); err != nil {
		t.Fatal(err)
	}
	return state, version
}

func openRawRunsDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runs.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func applyRunsSQL(t *testing.T, db *sql.DB, names ...string) {
	t.Helper()
	for _, name := range names {
		body, err := runsMigrations.ReadFile("migrations/runs/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(body)); err != nil {
			t.Fatalf("执行 %s: %v", name, err)
		}
	}
}
