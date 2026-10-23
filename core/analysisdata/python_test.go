package analysisdata

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPythonDecoderLargeCellAndPrecision(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("fixed decoder compatibility requires Python 3")
	}
	payload := map[string]any{"columns": []string{"", "重复", "重复", "编码"}, "rows": [][]any{{strings.Repeat("界,\n\"", 40000), json.Number("9223372036854775807"), json.Number("1.2345678901234567"), map[string]string{"encoding": "base64", "data": "/wBh"}}, {"null", nil, "", json.Number("-9223372036854775808")}}, "row_count": 2, "truncated": false}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	files, err := FromToolJSON(t.Context(), string(data))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for name, body := range files {
		if err = os.WriteFile(filepath.Join(root, name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Fixed test source loads the same helper shipped to the sandbox; no table
	// content is interpolated into Python source or executed as code.
	script := `import runpy,sys
from decimal import Decimal
load=runpy.run_path(sys.argv[1]+'/lantern_table.py')['load_table']
columns,rows,meta=load(sys.argv[1])
assert columns==['','重复','重复','编码']
assert meta['column_ids']==['c0','c1','c2','c3']
assert rows[0][0]=='界,\n"'*40000
assert rows[0][1]==9223372036854775807 and type(rows[0][1]) is int
assert rows[0][2]==Decimal('1.2345678901234567') and type(rows[0][2]) is Decimal
assert rows[0][3]=={'encoding':'base64','data':'/wBh'}
assert rows[1]==['null',None,'',-9223372036854775808]
print('large-cell-and-precision-preserved')`
	cmd := exec.CommandContext(t.Context(), python, "-I", "-B", "-c", script, root)
	output, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != "large-cell-and-precision-preserved" {
		t.Fatalf("fixed decoder: error=%v output=%s", err, output)
	}
}
func TestExactSourceByteBoundary(t *testing.T) {
	source := `{"columns":["a"],"rows":[],"row_count":0,"truncated":false}`
	padded := strings.Repeat(" ", MaxSourceBytes-len(source)) + source
	if _, err := FromToolJSON(t.Context(), padded); err != nil {
		t.Fatal(err)
	}
}
