package seed_test

import (
	"bytes"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/workspace"
	"github.com/loveRyujin/lantern/internal/seed"

	_ "modernc.org/sqlite"
)

// 种子 20261021 的内容哈希。生成逻辑或 Go 的 math/rand/v2 一变，评测里的标准答案就会失效。
const (
	goldenHash    = "1a7538f9d00c7631a8ba3da0565f2e7ae05b73d24cf1c63ce8b18d6f68fb9de9"
	goldenXinghe  = "9991812eff9d0a7dc44702681a7c66bcfd3c9fb59f8a4ac146a826543b193127"
	goldenYunshan = "b639c10bceb11cf023c9b54dbb6310121e247e1657775abd85726f2752b70965"
)

func TestGoldenHash(t *testing.T) {
	if seed.DefaultSeed != 20261021 {
		t.Fatalf("DefaultSeed = %d，黄金哈希按种子 20261021 冻结。请更新评测标准答案，并同时改这个黄金哈希。", seed.DefaultSeed)
	}
	res := mustRun(t, t.TempDir(), 20261021, false)
	gotXinghe, gotYunshan := "", ""
	for _, ws := range res.Workspaces {
		switch ws.ID {
		case "星禾":
			gotXinghe = ws.Hash
		case "云杉":
			gotYunshan = ws.Hash
		}
	}
	if res.Hash == goldenHash && gotXinghe == goldenXinghe && gotYunshan == goldenYunshan {
		return
	}
	t.Fatalf("种子 20261021 的内容哈希变了。\n内容哈希 got %s\n         want %s\n星禾 got %s\n     want %s\n云杉 got %s\n     want %s\n生成逻辑或 Go 版本（math/rand/v2）变化会使评测标准答案失效。请更新评测标准答案，并同时改这个黄金哈希。",
		res.Hash, goldenHash, gotXinghe, goldenXinghe, gotYunshan, goldenYunshan)
}

func TestSameSeedSameHash(t *testing.T) {
	first := mustRun(t, t.TempDir(), seed.DefaultSeed, false)
	second := mustRun(t, t.TempDir(), seed.DefaultSeed, false)
	if first.Hash == "" || len(first.Hash) != 64 {
		t.Fatalf("hash %q", first.Hash)
	}
	if first.Hash != second.Hash {
		t.Fatalf("same seed hash %s vs %s", first.Hash, second.Hash)
	}
	if len(first.Workspaces) != 2 || len(second.Workspaces) != 2 {
		t.Fatalf("workspaces %d", len(first.Workspaces))
	}
	for i := range first.Workspaces {
		if first.Workspaces[i].Hash != second.Workspaces[i].Hash {
			t.Fatalf("%s hash changed", first.Workspaces[i].ID)
		}
	}
	other := mustRun(t, t.TempDir(), seed.DefaultSeed+1, false)
	if other.Hash == first.Hash {
		t.Fatal("different seed produced the same hash")
	}
}

func TestAnalyticsHasNoPlainContacts(t *testing.T) {
	res := mustRun(t, t.TempDir(), seed.DefaultSeed, false)
	for _, ws := range res.Workspaces {
		phones, emails := readContacts(t, filepath.Join(ws.Dir, "sensitive.db"))
		if len(phones) != ws.Users || len(emails) != ws.Users {
			t.Fatalf("%s contacts phone=%d email=%d users=%d", ws.ID, len(phones), len(emails), ws.Users)
		}
		// SQLite 文件头含有字节 0x40，整文件搜 "@" 会误报。
		// 这里比的是敏感库里的整段手机号和邮箱。
		blob := readAnalyticsBytes(t, ws.Dir)
		for _, phone := range phones {
			if bytes.Contains(blob, []byte(phone)) {
				t.Fatalf("%s analytics.db contains a phone number", ws.ID)
			}
		}
		for _, email := range emails {
			if bytes.Contains(blob, []byte(email)) {
				t.Fatalf("%s analytics.db contains an email", ws.ID)
			}
		}
		assertAnalyticsColumnsClean(t, ws.Dir)
	}
}

func TestDatasetShape(t *testing.T) {
	res := mustRun(t, t.TempDir(), seed.DefaultSeed, false)
	if res.Workspaces[0].ID != "星禾" || res.Workspaces[1].ID != "云杉" {
		t.Fatalf("ids %s %s", res.Workspaces[0].ID, res.Workspaces[1].ID)
	}
	want := map[string]struct{ users, products, orders int }{
		"星禾": {240, 48, 3600},
		"云杉": {96, 30, 1200},
	}
	if res.Workspaces[0].Hash == res.Workspaces[1].Hash {
		t.Fatal("workspaces hashed the same")
	}
	for _, ws := range res.Workspaces {
		exp := want[ws.ID]
		if ws.Users != exp.users || ws.Products != exp.products || ws.Orders != exp.orders {
			t.Fatalf("%s counts users=%d products=%d orders=%d", ws.ID, ws.Users, ws.Products, ws.Orders)
		}
		if ws.OrderItems < ws.Orders || ws.OrderItems > ws.Orders*3 || ws.Refunds <= 0 {
			t.Fatalf("%s items=%d refunds=%d", ws.ID, ws.OrderItems, ws.Refunds)
		}
		db := openSQLite(t, filepath.Join(ws.Dir, "analytics.db"))
		var badAmount int
		if err := db.QueryRowContext(t.Context(), `
			SELECT COUNT(*) FROM orders o
			WHERE amount_cents != (
				SELECT COALESCE(SUM(qty * unit_price_cents), 0)
				FROM order_items WHERE order_id = o.id
			)
		`).Scan(&badAmount); err != nil {
			t.Fatal(err)
		}
		if badAmount != 0 {
			t.Fatalf("%s orders with amount mismatch: %d", ws.ID, badAmount)
		}
		var fkViolations int
		if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&fkViolations); err != nil {
			t.Fatal(err)
		}
		if fkViolations != 0 {
			t.Fatalf("%s foreign_key_check violations: %d", ws.ID, fkViolations)
		}
		var badRefund int
		if err := db.QueryRowContext(t.Context(), `
			SELECT COUNT(*) FROM refunds r
			LEFT JOIN orders o ON o.id = r.order_id
			WHERE o.id IS NULL
			   OR o.status != '已支付'
			   OR r.amount_cents <= 0
			   OR r.amount_cents > o.amount_cents
			   OR r.created_at < o.created_at
		`).Scan(&badRefund); err != nil {
			t.Fatal(err)
		}
		if badRefund != 0 {
			t.Fatalf("%s bad refunds: %d", ws.ID, badRefund)
		}
		var orphanItems int
		if err := db.QueryRowContext(t.Context(), `
			SELECT COUNT(*) FROM order_items i
			LEFT JOIN orders o ON o.id = i.order_id
			LEFT JOIN products p ON p.id = i.product_id
			WHERE o.id IS NULL OR p.id IS NULL
		`).Scan(&orphanItems); err != nil {
			t.Fatal(err)
		}
		if orphanItems != 0 {
			t.Fatalf("%s orphan order items: %d", ws.ID, orphanItems)
		}
		var minAt, maxAt string
		if err := db.QueryRowContext(t.Context(), `SELECT MIN(created_at), MAX(created_at) FROM orders`).Scan(&minAt, &maxAt); err != nil {
			t.Fatal(err)
		}
		if minAt < "2025-10-01T00:00:00Z" || maxAt >= "2026-10-01T00:00:00Z" {
			t.Fatalf("%s order window %s .. %s", ws.ID, minAt, maxAt)
		}
		minTime, err := time.Parse(time.RFC3339, minAt)
		if err != nil {
			t.Fatal(err)
		}
		maxTime, err := time.Parse(time.RFC3339, maxAt)
		if err != nil {
			t.Fatal(err)
		}
		if maxTime.Sub(minTime) < 330*24*time.Hour {
			t.Fatalf("%s span %s", ws.ID, maxTime.Sub(minTime))
		}
	}
}

func TestForceRebuildAndRejectExisting(t *testing.T) {
	root := t.TempDir()
	first := mustRun(t, root, seed.DefaultSeed, false)
	_, err := seed.Run(t.Context(), seed.Options{Root: root, Seed: seed.DefaultSeed, Force: false})
	if !errors.Is(err, workspace.ErrExists) {
		t.Fatalf("second Run: %v", err)
	}
	second := mustRun(t, root, seed.DefaultSeed, true)
	if second.Hash != first.Hash {
		t.Fatalf("force rebuild hash %s vs %s", second.Hash, first.Hash)
	}
}

func TestForceRejectsSymlinks(t *testing.T) {
	t.Run("workspaces", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		secret := filepath.Join(outside, "星禾", "keep.txt")
		if err := os.MkdirAll(filepath.Dir(secret), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(secret, []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "workspaces")); err != nil {
			t.Fatal(err)
		}
		_, err := seed.Run(t.Context(), seed.Options{Root: root, Seed: seed.DefaultSeed, Force: true})
		if err == nil || !strings.Contains(err.Error(), "符号链接") {
			t.Fatalf("Run: %v", err)
		}
		if _, err := os.Stat(secret); err != nil {
			t.Fatalf("symlink target was removed: %v", err)
		}
	})

	t.Run("workspace", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		secret := filepath.Join(outside, "keep.txt")
		if err := os.WriteFile(secret, []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
		workspaces := filepath.Join(root, "workspaces")
		if err := os.Mkdir(workspaces, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(workspaces, "星禾")); err != nil {
			t.Fatal(err)
		}
		_, err := seed.Run(t.Context(), seed.Options{Root: root, Seed: seed.DefaultSeed, Force: true})
		if err == nil || !strings.Contains(err.Error(), "符号链接") {
			t.Fatalf("Run: %v", err)
		}
		if _, err := os.Stat(secret); err != nil {
			t.Fatalf("symlink target was removed: %v", err)
		}
		info, err := os.Lstat(filepath.Join(workspaces, "星禾"))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Fatal("workspace symlink was replaced")
		}
	})
}

func TestSeedApprovalID(t *testing.T) {
	if seed.SeedApprovalID != "lantern-seed" {
		t.Fatalf("approval id %q", seed.SeedApprovalID)
	}
	if strings.ContainsFunc(seed.SeedApprovalID, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n'
	}) {
		t.Fatalf("approval id %q has whitespace", seed.SeedApprovalID)
	}
}

func TestDataDocuments(t *testing.T) {
	root := filepath.Join("..", "..", "data")
	xingheGMV := "已支付订单后来发生退款时，GMV 仍按支付金额全额计入"
	yunshanGMV := "GMV 等于已支付订单金额减去这些订单的退款金额"
	docs := []struct {
		path    string
		needles []string
	}{
		{"星禾/指标口径手册.md", []string{xingheGMV, "净销售额", "客单价", "复购率", "退款率", "华东", "上海"}},
		{"云杉/指标口径手册.md", []string{yunshanGMV, "净销售额", "客单价", "复购率", "退款率", "华东"}},
		{"星禾/数据字典.md", []string{"orders", "order_items", "products", "users", "refunds", "sensitive.db", "phone", "email", "20261021", "2025-10-01", "240", "3600"}},
		{"云杉/数据字典.md", []string{"orders", "order_items", "products", "users", "refunds", "sensitive.db", "phone", "email", "20261021", "96", "1200"}},
		{"星禾/数据安全规范.md", []string{"phone", "email", "sensitive.db", "user_contacts", "确认", "星禾", "ConfirmExport", "lantern-seed", "export_data", "工作区启动时不打开这个库"}},
		{"云杉/数据安全规范.md", []string{"phone", "email", "sensitive.db", "user_contacts", "确认", "云杉", "ConfirmExport", "lantern-seed", "export_data", "工作区启动时不打开这个库"}},
	}
	for _, doc := range docs {
		body, err := os.ReadFile(filepath.Join(root, doc.path))
		if err != nil {
			t.Fatal(err)
		}
		if len(body) < 400 {
			t.Fatalf("%s is too short (%d bytes)", doc.path, len(body))
		}
		text := string(body)
		for _, needle := range doc.needles {
			if !strings.Contains(text, needle) {
				t.Fatalf("%s missing %q", doc.path, needle)
			}
		}
		if strings.Contains(doc.path, "数据安全规范.md") && strings.Contains(text, "已经打开的敏感库") {
			t.Fatalf("%s still says the seed opens sensitive.db at startup", doc.path)
		}
	}
	xh, err := os.ReadFile(filepath.Join(root, "星禾/指标口径手册.md"))
	if err != nil {
		t.Fatal(err)
	}
	ys, err := os.ReadFile(filepath.Join(root, "云杉/指标口径手册.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ys), xingheGMV) {
		t.Fatal("云杉口径写成了星禾的 GMV")
	}
	if strings.Contains(string(xh), yunshanGMV) {
		t.Fatal("星禾口径写成了云杉的 GMV")
	}
}

func mustRun(t *testing.T, root string, seedN uint64, force bool) seed.Result {
	t.Helper()
	res, err := seed.Run(t.Context(), seed.Options{Root: root, Seed: seedN, Force: force})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func readContacts(t *testing.T, path string) (phones, emails []string) {
	t.Helper()
	db := openSQLite(t, path)
	rows, err := db.QueryContext(t.Context(), `SELECT phone, email FROM user_contacts ORDER BY user_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	phoneRe := regexp.MustCompile(`^1[3-9]\d{9}$`)
	for rows.Next() {
		var phone, email string
		if err := rows.Scan(&phone, &email); err != nil {
			t.Fatal(err)
		}
		if !phoneRe.MatchString(phone) {
			t.Fatalf("phone %q", phone)
		}
		if !strings.Contains(email, "@") {
			t.Fatalf("email %q", email)
		}
		phones = append(phones, phone)
		emails = append(emails, email)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return phones, emails
}

func readAnalyticsBytes(t *testing.T, dir string) []byte {
	t.Helper()
	var buf []byte
	found := false
	for _, name := range []string{"analytics.db", "analytics.db-wal", "analytics.db-shm"} {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatal(err)
		}
		found = true
		buf = append(buf, body...)
	}
	if !found || len(buf) == 0 {
		t.Fatalf("analytics files missing in %s", dir)
	}
	return buf
}

func assertAnalyticsColumnsClean(t *testing.T, dir string) {
	t.Helper()
	db := openSQLite(t, filepath.Join(dir, "analytics.db"))
	names := sqliteTables(t, db)
	phoneRe := regexp.MustCompile(`1[3-9]\d{9}`)
	for _, table := range names {
		cols, err := db.QueryContext(t.Context(), `SELECT name, type FROM pragma_table_info(?)`, table)
		if err != nil {
			t.Fatal(err)
		}
		type col struct{ name, kind string }
		var textCols []col
		for cols.Next() {
			var c col
			if err := cols.Scan(&c.name, &c.kind); err != nil {
				cols.Close()
				t.Fatal(err)
			}
			if c.name == "phone" || c.name == "email" {
				cols.Close()
				t.Fatalf("analytics %s.%s", table, c.name)
			}
			if strings.EqualFold(c.kind, "TEXT") {
				textCols = append(textCols, c)
			}
		}
		if err := cols.Err(); err != nil {
			cols.Close()
			t.Fatal(err)
		}
		cols.Close()
		if len(textCols) == 0 {
			continue
		}
		quoted := make([]string, len(textCols))
		for i, c := range textCols {
			quoted[i] = quoteIdent(c.name)
		}
		query := "SELECT " + strings.Join(quoted, ", ") + " FROM " + quoteIdent(table)
		rows, err := db.QueryContext(t.Context(), query)
		if err != nil {
			t.Fatal(err)
		}
		vals := make([]sql.NullString, len(textCols))
		ptrs := make([]any, len(textCols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for rows.Next() {
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			for _, v := range vals {
				if !v.Valid {
					continue
				}
				if strings.Contains(v.String, "@") || phoneRe.MatchString(v.String) {
					rows.Close()
					t.Fatalf("analytics %s contains contact text", table)
				}
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		rows.Close()
	}
}

func sqliteTables(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `
		SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name
	`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func openSQLite(t *testing.T, path string) *sql.DB {
	t.Helper()
	dsn := url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	return db
}
