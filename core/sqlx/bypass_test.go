package sqlx

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/loveRyujin/lantern/core/tools"
)

// TestRunSQLBypassShapesStayBounded 把改写器的三条绕过收成回归。
// CTE 后面的 SELECT *、多表 FROM 里的子查询、以及用户自己写的 lantern_cell，
// 都在准备之后、第一步之前被拒绝，VmHWM 停在一条合法结果行的规模里。
// modernc 的分配在 libc 堆上。VmHWM 是整个进程的，所以这里不用 t.Parallel，
// 每个形状单独一个子进程。不打开 MEMSTATUS，也不设置硬堆上限。
func TestRunSQLBypassShapesStayBounded(t *testing.T) {
	id := os.Getenv("LANTERN_SQL_MEM_CHILD")
	if id == "" {
		for _, name := range bypassCaseNames() {
			t.Run(name, func(t *testing.T) {
				spawnMemChild(t, "TestRunSQLBypassShapesStayBounded", name)
			})
		}
		return
	}
	if !isBypassChild(id) {
		t.Skip("留给别的内存子进程")
	}
	probeBypassCase(t, id)
}

// TestIntermediateResultsUnbounded 记下 B6：这些形状会同时握住很多中间值。
// 不测内存上限。修复是 issue #38，把 run_sql 放进子进程，用 RLIMIT_AS 或 cgroup 限制。
func TestIntermediateResultsUnbounded(t *testing.T) {
	t.Skip("子查询、物化 CTE、GROUP BY、UNION、IN 列表和 ORDER BY 的中间结果没有内存上限，超时是现在的兜底。见 https://github.com/loveRyujin/lantern/issues/38")
}

func bypassCaseNames() []string {
	return []string{"cte-star", "join-subquery", "lantern-cell"}
}

func isBypassChild(id string) bool {
	for _, name := range bypassCaseNames() {
		if name == id {
			return true
		}
	}
	return false
}

func probeBypassCase(t *testing.T, id string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "analytics.db")
	tool := openMemTool(t, path)
	cols := wideExprs(40, "hex(zeroblob(2000000))")
	var query, want string
	switch id {
	case "cte-star":
		query = "WITH x AS (SELECT " + cols + ") SELECT * FROM x"
		want = "32"
	case "join-subquery":
		query = "SELECT * FROM (SELECT 1 AS k) a, (SELECT " + cols + ") b"
		want = "32"
	case "lantern-cell":
		query = "SELECT " + wideExprs(40, "lantern_cell(0, hex(zeroblob(2000000)))")
		want = "lantern_cell"
	default:
		t.Fatalf("未知的内存用例 %s", id)
	}
	before := processHWM(t)
	res, err := tool.Execute(t.Context(), sqlCall(t, query))
	delta := processHWM(t) - before
	t.Logf("VmHWM delta=%d err=%v", delta, err)
	var te *tools.Error
	if !errors.As(err, &te) || te.Code != CodeRejected || te.Retryable || res.Content != "" {
		t.Fatalf("err=%v res=%+v", err, res)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("错误里没有 %q: %v", want, err)
	}
	// 列数在第一步之前就拒绝了，这些值不会进寄存器。没有硬堆上限也能停在一条合法结果行的规模里。
	const bound = MaxResultColumns*MaxValueBytes + 8<<20
	if delta > bound {
		t.Fatalf("VmHWM 增加 %d，超过 32 列 × 4 MiB（%d）", delta, bound)
	}
}

func openMemTool(t *testing.T, path string) *Tool {
	t.Helper()
	createEmptyDB(t, path)
	db, err := Open(t.Context(), path, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tool, err := NewTool(db, path, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	return tool
}

// spawnMemChild 在干净的子进程里跑一个内存用例。
// VmHWM 是进程一辈子的峰值，清不掉，也不能和别的测试挤在一起，更不能 t.Parallel。
func spawnMemChild(t *testing.T, testName, childID string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+regexp.QuoteMeta(testName)+"$", "-test.count=1", "-test.v")
	env := make([]string, 0, len(os.Environ())+1)
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "LANTERN_SQL_MEM_CHILD=") {
			continue
		}
		env = append(env, e)
	}
	cmd.Env = append(env, "LANTERN_SQL_MEM_CHILD="+childID)
	out, err := cmd.CombinedOutput()
	t.Log(string(out))
	if err != nil {
		t.Fatalf("内存子进程 %s 失败: %v", childID, err)
	}
}

func wideExprs(n int, expr string) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = expr
	}
	return strings.Join(parts, ", ")
}

func sqlCall(t *testing.T, query string) tools.Call {
	t.Helper()
	body, err := json.Marshal(map[string]string{"sql": query})
	if err != nil {
		t.Fatal(err)
	}
	return tools.Call{Args: body}
}
