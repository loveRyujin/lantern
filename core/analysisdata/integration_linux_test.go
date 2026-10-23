//go:build linux && amd64

package analysisdata_test

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/analysisdata"
	"github.com/loveRyujin/lantern/core/sandbox"
	"github.com/loveRyujin/lantern/core/sqlx"
	"github.com/loveRyujin/lantern/core/tools"
	"github.com/loveRyujin/lantern/core/workspace"
)

func dataQAFixture(t *testing.T) (*sqlx.Tool, *sandbox.Runner, string, string) {
	t.Helper()
	ws, err := workspace.Create(t.Context(), t.TempDir(), "analysis-qa")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	if _, err := ws.Analytics.ExecContext(t.Context(), "CREATE TABLE qa_values (big INTEGER, text TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Analytics.ExecContext(t.Context(), "INSERT INTO qa_values VALUES (?, ?)", int64(9223372036854775807), "引号\"逗号,\r\n换行🙂e\u0301"); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(ws.Dir, "analytics.db")
	db, err := sqlx.Open(t.Context(), dbPath, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tool, err := sqlx.NewTool(db, dbPath, dbPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(ws.Dir, "tmp")
	runner := sandbox.New(sandbox.Config{TempRoot: root})
	probe, err := runner.Run(t.Context(), sandbox.Spec{Code: "print('analysis-qa-ready')"})
	if errors.Is(err, sandbox.ErrSandboxUnavailable) || errors.Is(err, sandbox.ErrRuntimeUnavailable) {
		if os.Getenv("SANDBOX_REQUIRE_REAL") != "1" {
			t.Skipf("real sandbox unavailable: %v", err)
		}
	}
	if err != nil || probe.Stdout != "analysis-qa-ready\n" {
		t.Fatalf("real sandbox preflight: result=%+v error=%v", probe, err)
	}
	dataQAClean(t, root)
	return tool, runner, root, dbPath
}

func dataQAClean(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("workspace execution root not empty: entries=%v error=%v", entries, err)
	}
}

func dataQASQL(t *testing.T, tool *sqlx.Tool, query string, limit int) tools.Result {
	t.Helper()
	args, _ := json.Marshal(map[string]any{"sql": query, "limit": limit})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	result, err := tool.Execute(ctx, tools.Call{ID: "real-data-query", RunID: "analysis-data-qa", Args: args})
	if err != nil {
		t.Fatalf("production SQL worker: %v", err)
	}
	return result
}

func dataQAInputs(t *testing.T, content string) map[string][]byte {
	t.Helper()
	inputs, err := analysisdata.FromToolJSON(t.Context(), content)
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 3 || string(inputs["lantern_table.py"]) != analysisdata.PythonDecoder {
		t.Fatalf("missing fixed converter inputs: %v", reflect.ValueOf(inputs).MapKeys())
	}
	return inputs
}

func dataQARun(t *testing.T, runner *sandbox.Runner, spec sandbox.Spec) (sandbox.Result, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	return runner.Run(ctx, spec)
}

const dataQALoad = "import os,runpy,json,csv\ncolumns,rows,meta=runpy.run_path(os.environ['INPUT_DIR']+'/lantern_table.py')['load_table']()\n"

func dataQAArtifact(t *testing.T, result sandbox.Result, name string) []byte {
	t.Helper()
	for _, artifact := range result.Artifacts {
		if artifact.Name == name {
			if err := sandbox.ValidateArtifact(artifact, 0); err != nil {
				t.Fatalf("returned invalid artifact: %v", err)
			}
			return artifact.Data
		}
	}
	t.Fatalf("missing artifact %s: %+v", name, result)
	return nil
}

func dataQAJSON(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var value map[string]any
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := d.Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestAnalysisDataQARealSQLToSandboxArtifacts(t *testing.T) {
	tool, runner, root, dbPath := dataQAFixture(t)
	query := `SELECT big AS dup, -9223372036854775808 AS dup, 9007199254740993 AS "", NULL AS n, '' AS empty,
'null' AS literal, text AS "逗号,
列", CAST(x'80ff' AS TEXT) AS invalid, x'0001ff' AS blob, x'' AS emptyblob,
0.1 AS decimal, '=1+1' AS formula, char(0)||'tail' AS control FROM qa_values`
	source := dataQASQL(t, tool, query, 1000)
	inputs := dataQAInputs(t, source.Content)
	header, err := csv.NewReader(bytes.NewReader(inputs["table.csv"])).Read()
	if err != nil || len(header) != 13 || header[0] != "c0" || header[12] != "c12" {
		t.Fatalf("positional CSV header=%v error=%v", header, err)
	}
	metadata := dataQAJSON(t, inputs["table.meta.json"])
	var ids []string
	columnIDs, ok := metadata["column_ids"].([]any)
	if !ok {
		t.Fatalf("metadata has no ordered column IDs: %v", metadata)
	}
	for _, id := range columnIDs {
		text, ok := id.(string)
		if !ok {
			t.Fatalf("metadata column ID is not text: %v", id)
		}
		ids = append(ids, text)
	}
	if metadata["schema_version"] != json.Number("1") || metadata["cell_encoding"] != "json" || !reflect.DeepEqual(ids, header) {
		t.Fatalf("metadata does not describe positional JSON-cell CSV: %v", metadata)
	}
	for name, data := range inputs {
		if bytes.Contains(data, []byte(dbPath)) || bytes.Contains(data, []byte(query)) {
			t.Fatalf("input %s contains host database path or raw SQL", name)
		}
	}
	code := dataQALoad + fmt.Sprintf(`from decimal import Decimal
assert columns[:3]==['dup','dup',''] and columns[6]=='逗号,\n列',columns
assert len(rows)==1 and len(rows[0])==13
r=rows[0]
assert r[0]==9223372036854775807 and type(r[0]) is int
assert r[1]==-9223372036854775808 and r[2]==9007199254740993
assert r[3] is None and r[4]=='' and r[5]=='null'
assert r[6]=='引号"逗号,\r\n换行🙂e\u0301'
assert r[7]=={'encoding':'base64','data':'gP8='} and r[8]=='AAH/' and r[9]==''
assert type(r[10]) is Decimal and r[10]==Decimal('0.1')
assert r[11]=='=1+1' and r[12]=='\x00tail'
for name in ['table.csv','table.meta.json','lantern_table.py']:
 try: open(os.environ['INPUT_DIR']+'/'+name,'ab')
 except PermissionError: pass
 else: raise AssertionError('readonly input writable: '+name)
try: open(%q,'rb').read()
except PermissionError: pass
else: raise AssertionError('business DB exposed to Python')
assert os.path.commonpath([os.environ['INPUT_DIR'],%q])==%q
out=os.environ['OUTPUT_DIR']
with open(out+'/result.json','w',encoding='utf-8') as f:
 json.dump({'columns':columns,'big':r[0],'small':r[1],'exact':r[2],'meta':meta,'text':r[6],'encoded':r[7],'decimal':str(r[10])},f,ensure_ascii=False)
with open(out+'/result.csv','w',encoding='utf-8',newline='') as f:
 w=csv.writer(f); w.writerow(['stat','value']); w.writerow(['rows',len(rows)]); w.writerow(['big',r[0]])
import struct,zlib
def chunk(kind,body):
 return struct.pack('>I',len(body))+kind+body+struct.pack('>I',zlib.crc32(kind+body)&0xffffffff)
pixels=bytes([0,len(rows),r[0]%%256,17])
png=b'\x89PNG\r\n\x1a\n'+chunk(b'IHDR',struct.pack('>IIBBBBB',1,1,8,2,0,0,0))+chunk(b'IDAT',zlib.compress(pixels))+chunk(b'IEND',b'')
open(out+'/result.png','wb').write(png)
print('real-chain-complete')
`, dbPath, root, root)
	result, err := dataQARun(t, runner, sandbox.Spec{Code: code, Inputs: inputs})
	if err != nil || result.Stdout != "real-chain-complete\n" || len(result.Artifacts) != 3 {
		t.Fatalf("production SQL→CSV→sandbox chain: result=%+v error=%v", result, err)
	}
	dataQAClean(t, root)
	value := dataQAJSON(t, dataQAArtifact(t, result, "result.json"))
	if value["big"] != json.Number("9223372036854775807") || value["small"] != json.Number("-9223372036854775808") || value["exact"] != json.Number("9007199254740993") || value["decimal"] != "0.1" {
		t.Fatalf("lossy result values: %#v", value)
	}
	sourceValue := dataQAJSON(t, []byte(source.Content))
	if !reflect.DeepEqual(value["columns"], sourceValue["columns"]) {
		t.Fatalf("duplicate/empty column names lost: %#v", value)
	}
	image, err := png.Decode(bytes.NewReader(dataQAArtifact(t, result, "result.png")))
	if err != nil || image.Bounds().Dx() != 1 || image.Bounds().Dy() != 1 {
		t.Fatalf("real PNG=%v error=%v", image, err)
	}
	r, g, b, _ := image.At(0, 0).RGBA()
	if r != 0x101 || g != 0xffff || b != 0x1111 {
		t.Fatalf("PNG does not encode actual row values: r=%x g=%x b=%x", r, g, b)
	}
	records, err := csv.NewReader(bytes.NewReader(dataQAArtifact(t, result, "result.csv"))).ReadAll()
	if err != nil || !reflect.DeepEqual(records, [][]string{{"stat", "value"}, {"rows", "1"}, {"big", "9223372036854775807"}}) {
		t.Fatalf("real CSV=%v error=%v", records, err)
	}
	// The returned bytes must survive cleanup and a subsequent execution.
	before, _ := json.Marshal(result.Artifacts)
	recovery, err := dataQARun(t, runner, sandbox.Spec{Code: "print('next-run')"})
	after, _ := json.Marshal(result.Artifacts)
	if err != nil || recovery.Stdout != "next-run\n" || !bytes.Equal(before, after) {
		t.Fatalf("result ownership after cleanup: result=%+v error=%v", recovery, err)
	}
	dataQAClean(t, root)
}

func TestAnalysisDataQARealTruncationAndLargeCells(t *testing.T) {
	tool, runner, root, _ := dataQAFixture(t)
	cases := []struct {
		name, query string
		limit       int
		truncated   bool
		reason      string
	}{
		{"empty", "SELECT big FROM qa_values WHERE 0", 1000, false, ""},
		{"exact-row-limit", "SELECT big FROM qa_values", 1, false, ""},
		{"row-truncation", "WITH RECURSIVE x(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM x WHERE n<4) SELECT n FROM x", 2, true, ""},
		{"bytes-truncation", "WITH RECURSIVE x(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM x WHERE n<10) SELECT n,hex(zeroblob(100000)) FROM x", 1000, true, "bytes"},
		{"large-json-cell", "SELECT printf('%0200000d',7) AS payload", 1000, false, ""},
		{"large-real-json-integer-token", "SELECT 1.0e20 AS payload", 1000, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := dataQASQL(t, tool, tc.query, tc.limit)
			original := dataQAJSON(t, []byte(source.Content))
			if original["truncated"] != tc.truncated || (tc.reason != "" && original["truncate_reason"] != tc.reason) {
				t.Fatalf("did not exercise real SQL truncation: %s", source.Content)
			}
			inputs := dataQAInputs(t, source.Content)
			code := dataQALoad + "\nopen(os.environ['OUTPUT_DIR']+'/metadata.json','w').write(json.dumps(meta))\nprint('decoded',len(rows))\n"
			if tc.name == "large-json-cell" {
				code += "assert len(rows[0][0])==200000 and rows[0][0].endswith('7')\n"
			} else if tc.name == "large-real-json-integer-token" {
				code += "assert rows[0][0]==100000000000000000000\n"
			}
			result, err := dataQARun(t, runner, sandbox.Spec{Code: code, Inputs: inputs})
			if err != nil || !strings.Contains(result.Stdout, "decoded ") {
				t.Fatalf("real decoder: result=%+v error=%v", result, err)
			}
			meta := dataQAJSON(t, dataQAArtifact(t, result, "metadata.json"))
			for _, key := range []string{"columns", "row_count", "truncated"} {
				if !reflect.DeepEqual(meta[key], original[key]) {
					t.Fatalf("metadata %s=%v, SQL=%v", key, meta[key], original[key])
				}
			}
			if tc.reason != "" && meta["truncate_reason"] != tc.reason {
				t.Fatalf("lost byte truncation reason: %v", meta)
			}
			dataQAClean(t, root)
		})
	}
}

func TestAnalysisDataQAInvalidSourceIsAtomic(t *testing.T) {
	cases := []string{
		`{"columns":[null],"rows":[],"row_count":0,"truncated":false}`,
		`{"columns":["a"],"rows":[],"row_count":null,"truncated":false}`,
		`{"columns":["a"],"rows":[],"row_count":0,"truncated":null}`,
		`{"columns":["a"],"rows":[[1,2]],"row_count":1,"truncated":false}`,
		`{"columns":["a"],"rows":[[1]],"row_count":0,"truncated":false}`,
		`{"columns":["a"],"rows":[],"row_count":0,"truncated":false,"truncate_reason":"bytes"}`,
		`{"columns":["a"],"rows":[],"row_count":0,"truncated":false,"row_count":1}`,
		`{"columns":["a"],"rows":[["\ud800"]],"row_count":1,"truncated":false}`,
		`{"columns":["a"],"rows":[[true]],"row_count":1,"truncated":false}`,
		`{"columns":["a"],"rows":[[{"encoding":"base64","data":"not-base64"}]],"row_count":1,"truncated":false}`,
	}
	for i, content := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			inputs, err := analysisdata.FromToolJSON(t.Context(), content)
			if !errors.Is(err, analysisdata.ErrInvalidSource) || inputs != nil {
				t.Fatalf("invalid source delivered partial inputs=%v error=%v", inputs, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	inputs, err := analysisdata.FromToolJSON(ctx, `{"columns":["a"],"rows":[],"row_count":0,"truncated":false}`)
	if !errors.Is(err, context.Canceled) || inputs != nil {
		t.Fatalf("canceled conversion delivered inputs=%v error=%v", inputs, err)
	}
}

func TestAnalysisDataQARealFailuresCleanupAndRecovery(t *testing.T) {
	tool, runner, root, _ := dataQAFixture(t)
	inputs := dataQAInputs(t, dataQASQL(t, tool, "SELECT big FROM qa_values", 1000).Content)
	cases := []struct {
		name, suffix string
		wall         time.Duration
		want         error
	}{
		{"python-error", "raise ValueError('controlled-data-failure')", 0, sandbox.ErrFailed},
		{"invalid-output", "open(os.environ['OUTPUT_DIR']+'/bad.csv','w').write('a,b\\n1\\n')", 0, sandbox.ErrFailed},
		{"timeout", "import time; time.sleep(10)", 100 * time.Millisecond, sandbox.ErrTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := dataQARun(t, runner, sandbox.Spec{Code: dataQALoad + "print('input-decoded',flush=True)\n" + tc.suffix, Inputs: inputs, Limits: sandbox.Limits{Wall: tc.wall}})
			if !errors.Is(err, tc.want) || len(result.Artifacts) != 0 {
				t.Fatalf("result=%+v error=%v want=%v", result, err, tc.want)
			}
			if tc.name != "timeout" && !strings.Contains(result.Stdout, "input-decoded") {
				t.Fatalf("failed before real decoder ran: %+v", result)
			}
			dataQAClean(t, root)
		})
	}
	// Exercise both cancellation during trusted staging and after Python has
	// decoded the table. Each path must finish cleanup before Run returns.
	for _, phase := range []string{"startup-cancel", "running-cancel"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			type outcome struct {
				result sandbox.Result
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := runner.Run(ctx, sandbox.Spec{Code: dataQALoad + "import time\nprint('cancel-ready',flush=True)\nopen(os.environ['OUTPUT_DIR']+'/ready.json','w').write('{}')\ntime.sleep(10)", Inputs: inputs})
				done <- outcome{result, err}
			}()
			pattern := filepath.Join(root, "*", "in", "table.csv")
			if phase == "running-cancel" {
				pattern = filepath.Join(root, "*", "out", "ready.json")
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				paths, _ := filepath.Glob(pattern)
				if len(paths) > 0 {
					break
				}
				if time.Now().After(deadline) {
					cancel()
					t.Fatal("real execution marker never appeared: " + phase)
				}
				time.Sleep(time.Millisecond)
			}
			cancel()
			select {
			case got := <-done:
				if !errors.Is(got.err, sandbox.ErrTimeout) || len(got.result.Artifacts) != 0 {
					t.Fatalf("canceled real chain: result=%+v error=%v", got.result, got.err)
				}
				if phase == "running-cancel" && !strings.Contains(got.result.Stdout, "cancel-ready") {
					t.Fatalf("canceled before Python executed: %+v", got.result)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("canceled execution was not reaped")
			}
			dataQAClean(t, root)
		})
	}
	result, err := dataQARun(t, runner, sandbox.Spec{Code: dataQALoad + "print(rows[0][0])", Inputs: inputs})
	if err != nil || result.Stdout != "9223372036854775807\n" {
		t.Fatalf("recovery with same SQL input: result=%+v error=%v", result, err)
	}
	dataQAClean(t, root)
}
