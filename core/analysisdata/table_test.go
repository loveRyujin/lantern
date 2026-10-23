package analysisdata

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestTransportPreservesObservableValues(t *testing.T) {
	source := `{"columns":["重复","重复","","line\nbreak","逗,号"],"rows":[[9223372036854775807,null,"","a,\n\"b\"\u0000",{"encoding":"base64","data":"/wBh"}],[-9223372036854775808,"null","YWJj",1.2345678901234567,0]],"row_count":2,"truncated":true,"truncate_reason":"bytes"}`
	files, err := FromToolJSON(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 || string(files["lantern_table.py"]) != PythonDecoder {
		t.Fatalf("unexpected files: %v", files)
	}
	var meta metadata
	if err = json.Unmarshal(files["table.meta.json"], &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Version != 1 || meta.CellEncoding != "json" || meta.Columns[0] != "重复" || meta.Columns[1] != "重复" || meta.Columns[2] != "" || meta.Columns[3] != "line\nbreak" || meta.Columns[4] != "逗,号" || !meta.Truncated || meta.TruncateReason != "bytes" || meta.RowCount != 2 || strings.Join(meta.ColumnIDs, ",") != "c0,c1,c2,c3,c4" {
		t.Fatalf("metadata=%+v", meta)
	}
	records, err := csv.NewReader(bytes.NewReader(files["table.csv"])).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(records[0], ",") != "c0,c1,c2,c3,c4" || len(records) != 3 {
		t.Fatalf("records=%q", records)
	}
	want := [][]string{{"9223372036854775807", "null", `""`, `"a,\n\"b\"\u0000"`, `{"data":"/wBh","encoding":"base64"}`}, {"-9223372036854775808", `"null"`, `"YWJj"`, "1.2345678901234567", "0"}}
	for r := range want {
		for c := range want[r] {
			if records[r+1][c] != want[r][c] {
				t.Fatalf("cell[%d,%d]=%s want %s", r, c, records[r+1][c], want[r][c])
			}
		}
	}
}
func TestTruncationAndEmptyTable(t *testing.T) {
	for _, suffix := range []string{`"truncated":false`, `"truncated":true`, `"truncated":true,"truncate_reason":"bytes"`} {
		files, err := FromToolJSON(t.Context(), `{"columns":["a"],"rows":[],"row_count":0,`+suffix+`}`)
		if err != nil {
			t.Fatal(err)
		}
		if string(files["table.csv"]) != "c0\n" {
			t.Fatalf("empty CSV=%q", files["table.csv"])
		}
	}
}
func TestRejectMalformedSourceAtomically(t *testing.T) {
	valid := `{"columns":["a"],"rows":[[1]],"row_count":1,"truncated":false}`
	cases := []string{"", `null`, `[]`, valid + `{}`, `{"error":"sql_rejected"}`,
		strings.Replace(valid, `"row_count":1`, `"row_count":2`, 1),
		strings.Replace(valid, `"row_count":1`, `"row_count":null`, 1),
		strings.Replace(valid, `"truncated":false`, `"truncated":null`, 1),
		strings.Replace(valid, `"truncated":false`, `"truncated":"false"`, 1),
		strings.Replace(valid, `"truncated":false`, `"truncated":false,"truncate_reason":null`, 1),
		strings.Replace(valid, `"truncated":false`, `"truncated":false,"truncate_reason":"bytes"`, 1),
		strings.Replace(valid, `"truncated":false`, `"truncated":true,"truncate_reason":"unknown"`, 1),
		strings.Replace(valid, `"columns":["a"]`, `"columns":[null]`, 1),
		strings.Replace(valid, `"columns":["a"]`, `"columns":null`, 1),
		strings.Replace(valid, `"columns":["a"]`, `"columns":[]`, 1),
		strings.Replace(valid, `"rows":[[1]]`, `"rows":[[1,2]]`, 1),
		strings.Replace(valid, `"rows":[[1]]`, `"rows":[null]`, 1),
		strings.Replace(valid, `"rows":[[1]]`, `"rows":null`, 1),
		strings.Replace(valid, `"columns":["a"]`, `"columns":["\ud800"]`, 1),
		strings.Replace(valid, `"columns":["a"]`, `"columns":["\udfff"]`, 1),
		strings.Replace(valid, `"columns":["a"]`, `"columns":["\ud800\ud800"]`, 1),
		strings.Replace(valid, `"row_count":1`, `"row_count":1,"row_count":1`, 1),
		strings.Replace(valid, `"rows":[[1]]`, `"rows":[["`+string([]byte{0xff})+`"]]`, 1),
		strings.Replace(valid, `"rows":[[1]]`, `"rows":[[true]]`, 1),
		strings.Replace(valid, `"rows":[[1]]`, `"rows":[[[1,2]]]`, 1),
		strings.Replace(valid, `"rows":[[1]]`, `"rows":[[{"arbitrary":"object"}]]`, 1),
		strings.Replace(valid, `"rows":[[1]]`, `"rows":[[{"encoding":"base64","data":"YQ==","data":"Yg=="}]]`, 1),
		strings.Replace(valid, `"rows":[[1]]`, `"rows":[[{"encoding":"base64","data":"invalid"}]]`, 1),
		strings.Replace(valid, `"rows":[[1]]`, `"rows":[[`+strings.Repeat("9", 65)+`]]`, 1),
		strings.Replace(valid, `"rows":[[1]]`, `"rows":[[1e9999]]`, 1),
		strings.Repeat(" ", MaxSourceBytes+1),
	}
	for i, body := range cases {
		files, err := FromToolJSON(t.Context(), body)
		if !errors.Is(err, ErrInvalidSource) || files != nil {
			t.Fatalf("case %d files=%v error=%v", i, files, err)
		}
	}
}
func TestShapeBoundaries(t *testing.T) {
	for _, test := range []struct {
		columns, rows int
		valid         bool
	}{{32, 1000, true}, {33, 0, false}, {1, 1001, false}} {
		cols := make([]string, test.columns)
		rows := make([][]any, test.rows)
		for i := range rows {
			rows[i] = make([]any, test.columns)
		}
		data, err := json.Marshal(map[string]any{"columns": cols, "rows": rows, "row_count": test.rows, "truncated": false})
		if err != nil {
			t.Fatal(err)
		}
		files, err := FromToolJSON(t.Context(), string(data))
		if test.valid {
			if err != nil {
				t.Fatal(err)
			}
			total := 0
			for _, v := range files {
				total += len(v)
			}
			if total > MaxInputBytes {
				t.Fatal("budget exceeded")
			}
		} else if !errors.Is(err, ErrInvalidSource) || files != nil {
			t.Fatalf("shape %+v files=%v error=%v", test, files, err)
		}
	}
}
func TestUnicodeSurrogatePairsAndLiteralBackslashes(t *testing.T) {
	files, err := FromToolJSON(t.Context(), `{"columns":["\ud83d\ude00"],"rows":[["\\ud800"]],"row_count":1,"truncated":false}`)
	if err != nil {
		t.Fatal(err)
	}
	var meta metadata
	if err = json.Unmarshal(files["table.meta.json"], &meta); err != nil || meta.Columns[0] != "😀" {
		t.Fatalf("meta=%+v error=%v", meta, err)
	}
	reader := csv.NewReader(bytes.NewReader(files["table.csv"]))
	_, _ = reader.Read()
	row, err := reader.Read()
	if err != nil || row[0] != `"\\ud800"` {
		t.Fatalf("row=%q error=%v", row, err)
	}
}
func TestCancellationAndReturnedFilesAreIndependent(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if files, err := FromToolJSON(ctx, `{}`); !errors.Is(err, context.Canceled) || files != nil {
		t.Fatalf("files=%v error=%v", files, err)
	}
	body := `{"columns":["x"],"rows":[[9007199254740993]],"row_count":1,"truncated":false}`
	first, err := FromToolJSON(t.Context(), body)
	if err != nil {
		t.Fatal(err)
	}
	first["table.csv"][0] = 'X'
	first["lantern_table.py"][0] = 'X'
	second, err := FromToolJSON(t.Context(), body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(second["table.csv"], []byte("c0\n")) || string(second["lantern_table.py"]) != PythonDecoder {
		t.Fatal("returned files alias another conversion")
	}
	if _, err = csv.NewReader(bytes.NewReader(second["table.csv"])).ReadAll(); err != nil && err != io.EOF {
		t.Fatal(err)
	}
}
func ExampleFromToolJSON() {
	files, err := FromToolJSON(context.Background(), `{"columns":["amount","amount"],"rows":[[9007199254740993,null]],"row_count":1,"truncated":false}`)
	if err != nil {
		panic(err)
	}
	fmt.Print(string(files["table.csv"]))
	// Output:
	// c0,c1
	// 9007199254740993,null
}

func TestIntegralRealBeyondInt64PreservesJSONToken(t *testing.T) {
	// encoding/json emits REAL 1e20 without a decimal/exponent. Source JSON has no
	// storage-class tag, so that valid REAL must not be rejected as an int64 overflow.
	source := `{"columns":["real"],"rows":[[100000000000000000000]],"row_count":1,"truncated":false}`
	files, err := FromToolJSON(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(bytes.NewReader(files["table.csv"])).ReadAll()
	if err != nil || rows[1][0] != "100000000000000000000" {
		t.Fatalf("rows=%q error=%v", rows, err)
	}
}
