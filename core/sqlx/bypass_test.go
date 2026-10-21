package sqlx

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/tools"
	"modernc.org/sqlite"
)

// orderByMemBound 是 100 行各 4 MiB 的 ORDER BY random() 允许的 VmHWM 峰值。
// 来自 temp_store=FILE 和 256 KiB 的 cache_size，不是堆上限。
const orderByMemBound = 256 << 20

// TestRunSQLBypassShapesStayBounded 把改写器的三条绕过收成回归，并挡住多行排序。
// CTE 后面的 SELECT *、多表 FROM 里的子查询、以及用户自己写的 lantern_cell，
// 都在准备之后、第一步之前被拒绝，VmHWM 停在一条合法结果行的规模里。
// 100 行各 4 MiB 的 ORDER BY random() 在自己的子进程里，VmHWM 峰值不超过大约 256 MiB。
// modernc 的分配在 libc 堆上。VmHWM 是整个进程的，所以这里不用 t.Parallel，
// 每个形状单独一个子进程。不打开 MEMSTATUS，也不设置硬堆上限。
func TestRunSQLBypassShapesStayBounded(t *testing.T) {
	id := os.Getenv("LANTERN_SQL_MEM_CHILD")
	if id == "" {
		for _, name := range bypassCaseNames() {
			t.Run(name, func(t *testing.T) {
				var extra []string
				if name == "order-by-many-rows" {
					// 灌库留在父进程。子进程的 VmHWM 只覆盖打开和排序，不把灌库的峰值算进去。
					path := filepath.Join(t.TempDir(), "analytics.db")
					createEmptyDB(t, path)
					fillSortRows(t, path)
					extra = append(extra, "LANTERN_SQL_SORT_DB="+path)
				}
				spawnMemChild(t, "TestRunSQLBypassShapesStayBounded", name, extra...)
			})
		}
		return
	}
	if !isBypassChild(id) {
		t.Skip("留给别的内存子进程")
	}
	probeBypassCase(t, id)
}

// TestIntermediateResultsUnbounded 记下仍没有内存上限的中间值。ORDER BY 不在这里。
// 修复是 W3 阻塞项。TODO(W3 issue #?)
func TestIntermediateResultsUnbounded(t *testing.T) {
	t.Skip("中间值没有内存上限。实测峰值：物化 CTE 1.19 GB，GROUP BY 2.74 GB，IN 列表 1.20 GB，UNION 也在同一范围。temp_store=FILE 和小缓存约束不住，超时也约束不住内存。ryujin 接受这个风险留在 W2。TODO(W3 issue #?)")
}

func bypassCaseNames() []string {
	return []string{"cte-star", "join-subquery", "lantern-cell", "order-by-many-rows"}
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
	if id == "order-by-many-rows" {
		probeSortMemory(t)
		return
	}
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

func probeSortMemory(t *testing.T) {
	t.Helper()
	sortPath := os.Getenv("LANTERN_SQL_SORT_DB")
	if sortPath == "" {
		t.Fatal("缺少 LANTERN_SQL_SORT_DB")
	}
	ctx := t.Context()
	sortDB, err := Open(ctx, sortPath, sortPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sortDB.Close() })
	sortTool, err := NewTool(sortDB, sortPath, sortPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(sortPath)
	before, err := listDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	var mu sync.Mutex
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				names, err := listDir(dir)
				if err != nil {
					continue
				}
				for name := range names {
					if _, ok := before[name]; ok {
						continue
					}
					mu.Lock()
					seen = append(seen, name)
					mu.Unlock()
				}
			}
		}
	}()
	info, err := os.Stat(sortPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeHWM := processHWM(t)
	res, err := sortTool.Execute(ctx, sqlCall(t, `SELECT body FROM t ORDER BY random()`))
	close(stop)
	<-done
	peak := processHWM(t)
	t.Logf("db=%d VmHWM before=%d peak=%d delta=%d bound=%d err=%v", info.Size(), beforeHWM, peak, peak-beforeHWM, orderByMemBound, err)
	var te *tools.Error
	if !errors.As(err, &te) || te.Code != CodeResultTooLarge || te.Retryable || res.Content != "" {
		t.Fatalf("排序 err=%v res=%+v", err, res)
	}
	if peak > orderByMemBound {
		t.Fatalf("VmHWM 峰值 %d 字节（约 %d MiB），超过大约 256 MiB（%d）。这个上限来自 temp_store=FILE 和 256 KiB cache_size，不是堆上限", peak, peak>>20, orderByMemBound)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) > 0 {
		t.Fatalf("业务库旁边多了临时文件: %v", seen)
	}
	after, err := listDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("查询后目录变了: before=%v after=%v", before, after)
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
func spawnMemChild(t *testing.T, testName, childID string, extra ...string) {
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
	cmd.Env = append(cmd.Env, extra...)
	out, err := cmd.CombinedOutput()
	t.Log(string(out))
	if err != nil {
		t.Fatalf("内存子进程 %s 失败: %v", childID, err)
	}
}

func fillSortRows(t *testing.T, path string) {
	t.Helper()
	u := url.URL{
		Scheme:   "file",
		Path:     filepath.ToSlash(path),
		RawQuery: url.Values{"_journal_mode": {"DELETE"}}.Encode(),
	}
	db := sql.OpenDB(hookConnector{dsn: u.String(), drv: &sqlite.Driver{}})
	defer db.Close()
	if _, err := db.Exec(`DROP TABLE t`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE t (body BLOB)`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for range 100 {
		if _, err := tx.Exec(`INSERT INTO t (body) VALUES (zeroblob(4194304))`); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func listDir(dir string) (map[string]struct{}, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		names[entry.Name()] = struct{}{}
	}
	return names, nil
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
