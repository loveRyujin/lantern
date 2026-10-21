package seed_test

import (
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/loveRyujin/lantern/core/workspace"
	"github.com/loveRyujin/lantern/internal/seed"
)

func TestExportEndToEndFromSeed(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	if _, err := seed.Run(ctx, seed.Options{Root: root, Seed: seed.DefaultSeed}); err != nil {
		t.Fatal(err)
	}
	h, err := workspace.Open(ctx, root, "星禾")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })

	const approvalID = "e2e-export"
	const object = "user_contacts"
	if err := seed.RecordExportApproval(ctx, h, approvalID, object); err != nil {
		t.Fatal(err)
	}

	grant, err := h.ConfirmExport(ctx, approvalID, object)
	if err != nil {
		t.Fatal(err)
	}
	db, err := h.OpenSensitive(ctx, grant)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	path := filepath.Join(h.Dir, "exports", "user_contacts.csv")
	exported := exportContacts(t, ctx, db, path)
	if len(exported) == 0 {
		t.Fatal("export wrote no contacts")
	}
	fromFile := readExport(t, path)
	if len(fromFile) != len(exported) {
		t.Fatalf("export file rows = %d, db rows = %d", len(fromFile), len(exported))
	}
	for i := range exported {
		if fromFile[i] != exported[i] {
			t.Fatalf("row %d file %#v db %#v", i, fromFile[i], exported[i])
		}
		if exported[i][1] == "" || exported[i][2] == "" {
			t.Fatalf("row %d missing phone or email: %#v", i, exported[i])
		}
	}

	again, err := h.ConfirmExport(ctx, approvalID, object)
	if !errors.Is(err, workspace.ErrExportUnconfirmed) {
		t.Fatalf("second confirm err = %v, want ErrExportUnconfirmed", err)
	}
	if again.ApprovalID() != "" {
		t.Fatalf("second confirm returned grant %q", again.ApprovalID())
	}
}

func exportContacts(t *testing.T, ctx context.Context, db *sql.DB, path string) [][3]string {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT user_id, phone, email FROM user_contacts ORDER BY user_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got [][3]string
	for rows.Next() {
		var row [3]string
		if err := rows.Scan(&row[0], &row[1], &row[2]); err != nil {
			t.Fatal(err)
		}
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	if err := w.Write([]string{"user_id", "phone", "email"}); err != nil {
		t.Fatal(err)
	}
	for _, row := range got {
		if err := w.Write(row[:]); err != nil {
			t.Fatal(err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return got
}

func readExport(t *testing.T, path string) [][3]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	records, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) == 0 || records[0][0] != "user_id" || records[0][1] != "phone" || records[0][2] != "email" {
		t.Fatalf("export header = %#v", records)
	}
	out := make([][3]string, 0, len(records)-1)
	for _, rec := range records[1:] {
		if len(rec) != 3 {
			t.Fatalf("export row %#v", rec)
		}
		out = append(out, [3]string{rec[0], rec[1], rec[2]})
	}
	return out
}
