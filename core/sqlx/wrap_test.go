package sqlx

import "testing"

func TestWrapResultCells(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{
			in:   "SELECT a, b FROM t",
			want: `SELECT lantern_cell(0, a) AS "a", lantern_cell(1, b) AS "b" FROM t`,
		},
		{
			in:   "SELECT length(body) FROM docs WHERE id = 1",
			want: `SELECT lantern_cell(0, length(body)) AS "length(body)" FROM docs WHERE id = 1`,
		},
		{
			in:   "SELECT city AS c FROM t",
			want: `SELECT lantern_cell(0, city) AS c FROM t`,
		},
		{
			in:   "SELECT * FROM t",
			want: "SELECT * FROM t",
		},
		{
			in:   "WITH c AS (SELECT hex(zeroblob(1)) AS h) SELECT length(h) FROM c",
			want: `WITH c AS (SELECT hex(zeroblob(1)) AS h) SELECT lantern_cell(0, length(h)) AS "length(h)" FROM c`,
		},
		{
			in:   "SELECT 'keep', 'a' UNION ALL SELECT hex(zeroblob(1)), hex(zeroblob(2))",
			want: `SELECT lantern_cell(0, 'keep') AS "'keep'", lantern_cell(1, 'a') AS "'a'" UNION ALL SELECT lantern_cell(0, hex(zeroblob(1))) AS "hex(zeroblob(1))", lantern_cell(1, hex(zeroblob(2))) AS "hex(zeroblob(2))"`,
		},
		{
			in:   "SELECT length(h) FROM (SELECT hex(zeroblob(1)) AS h)",
			want: `SELECT lantern_cell(0, length(h)) AS "length(h)" FROM (SELECT hex(zeroblob(1)) AS h)`,
		},
		{
			in:   "SELECT * FROM (SELECT hex(zeroblob(1)), hex(zeroblob(2)))",
			want: `SELECT * FROM (SELECT lantern_cell(0, hex(zeroblob(1))) AS "hex(zeroblob(1))", lantern_cell(1, hex(zeroblob(2))) AS "hex(zeroblob(2))")`,
		},
		{
			in:   "WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM c WHERE n < 3) SELECT n FROM c",
			want: `WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM c WHERE n < 3) SELECT lantern_cell(0, n) AS "n" FROM c`,
		},
	}
	for _, tc := range cases {
		got := wrapResultCells(tc.in)
		if got != tc.want {
			t.Errorf("in %s\ngot  %s\nwant %s", tc.in, got, tc.want)
		}
	}
}
