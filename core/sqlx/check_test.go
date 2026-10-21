package sqlx_test

import (
	"strings"
	"testing"

	"github.com/loveRyujin/lantern/core/sqlx"
)

func TestCheckAllowsReadOnlyShapes(t *testing.T) {
	cases := []struct {
		id  string
		sql string
	}{
		{"V-34-01", "SELECT 1;"},
		{"V-34-01-blank", "SELECT 1;  \n  -- tail"},
		{"V-34-01-empty-stmt", "SELECT 1; ;"},
		{"V-34-03", "SELECT 1 /* ; DROP TABLE orders */"},
		{"V-34-07", "sElEcT 1"},
		{"V-34-07-bom", "\ufeffSELECT 1"},
		{"V-34-07-fullwidth", "\u3000SELECT 1"},
		{"V-34-08", "SELECT '; DROP TABLE x; --'"},
		{"V-34-13", "SELECT * FROM sqlite_master"},
		{"V-34-13-temp", "SELECT name FROM sqlite_temp_master"},
		{"I-34-04", "WITH t AS (SELECT 1 AS n) SELECT n FROM t"},
		{"replace-func", "SELECT replace('a', 'a', 'b')"},
		{"union", "SELECT 1 UNION ALL SELECT 2"},
		{"quoted-delete", `SELECT * FROM "delete"`},
		{"pragma-in-string", "SELECT 'pragma_table_info'"},
		{"pragma-substring", `SELECT * FROM "pre_pragma_table"`},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			if err := sqlx.Check(tc.sql); err != nil {
				t.Fatalf("应放行: %v", err)
			}
		})
	}
}

func TestCheckRejectsWritesAndPragmas(t *testing.T) {
	long := "SELECT '" + strings.Repeat("a", sqlx.MaxSQLRunes) + "'"
	cases := []struct {
		id   string
		sql  string
		want string
	}{
		{"V-34-02", "SELECT 1; SELECT 2", "一条"},
		{"V-34-04", "SELECT 1 -- c\n; DELETE FROM orders", "一条"},
		{"V-34-05", "WITH x AS (DELETE FROM orders RETURNING *) SELECT * FROM x", "只读"},
		{"V-34-06", "WITH x AS (SELECT 1 AS id) INSERT INTO t SELECT * FROM x", "只读"},
		{"V-34-09", "EXPLAIN DELETE FROM orders", "SELECT"},
		{"V-34-09-plan", "EXPLAIN QUERY PLAN SELECT 1", "SELECT"},
		{"V-34-10", "SELECT load_extension('x')", "函数"},
		{"V-34-10-writefile", "SELECT writefile('x', 'y')", "函数"},
		{"V-34-10-readfile", "SELECT readfile('x')", "函数"},
		{"V-34-11", "SELECT * FROM pragma_table_info('users')", "schema"},
		{"V-34-11-query-only", "SELECT * FROM pragma_query_only", "schema"},
		{"V-34-11-dquote", `SELECT * FROM "pragma_table_info"('users')`, "schema"},
		{"V-34-11-bracket", "SELECT * FROM [pragma_table_info]('users')", "schema"},
		{"V-34-11-backtick", "SELECT * FROM `pragma_table_info`('users')", "schema"},
		{"V-34-11-dquote-case", `SELECT * FROM "Pragma_Table_Info"('users')`, "schema"},
		{"V-34-11-squote", "SELECT * FROM 'pragma_table_info'('users')", "schema"},
		{"V-34-11-squote-case", "SELECT * FROM 'Pragma_Table_Info'('users')", "schema"},
		{"V-34-12-blank", "   ", "空"},
		{"V-34-12-comment", "-- only", "空"},
		{"V-34-12-block", "/* c */", "空"},
		{"V-34-12-long", long, "长度"},
		{"pragma", "PRAGMA query_only=OFF", "schema"},
		{"pragma-comment", "/*x*/ pragma user_version", "schema"},
		{"attach", "ATTACH DATABASE 'a' AS b", "附加"},
		{"attach-lower", "attach database ':memory:' AS b", "附加"},
		{"detach", "DETACH DATABASE b", "附加"},
		{"values", "VALUES (1)", "SELECT"},
		{"nul", "SELECT 1\x00; DELETE FROM t", "非法"},
		{"unterminated-string", "SELECT 'abc", "不完整"},
		{"unterminated-comment", "SELECT 1 /*", "不完整"},
		{"with-without-select", "WITH x AS (SELECT 1) VALUES (1)", "SELECT"},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			err := sqlx.Check(tc.sql)
			if err == nil {
				t.Fatal("应当拒绝")
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误 %q 里没有 %q", err.Error(), tc.want)
			}
			if strings.Contains(err.Error(), "/") || strings.Contains(err.Error(), `\`) {
				t.Fatalf("拒绝信息像路径: %v", err)
			}
		})
	}
}

func TestCheckLengthBoundary(t *testing.T) {
	exact := "SELECT 1" + strings.Repeat(" ", sqlx.MaxSQLRunes-len("SELECT 1"))
	if utfLen(exact) != sqlx.MaxSQLRunes {
		t.Fatalf("构造长度 %d", utfLen(exact))
	}
	if err := sqlx.Check(exact); err != nil {
		t.Fatal(err)
	}
	if err := sqlx.Check(exact + " "); err == nil {
		t.Fatal("超过上限应当拒绝")
	}
}

func utfLen(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}
