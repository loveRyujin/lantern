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
	"modernc.org/libc"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// TestRunSQLBypassShapesStayBounded 把改写器的三条绕过收成回归，并加上多行排序。
// CTE 后面的 SELECT *、多表 FROM 里的子查询、以及用户自己写的 lantern_cell，
// sqlite3_memory_highwater 都不能超过大约 128 MiB。
// 100 行各 4 MiB 的 ORDER BY random() 不能超过大约 256 MiB。
// modernc 的分配在 libc 堆上，Go 的 TotalAlloc 不算数。
// sqlite3_memory_highwater 和 VmHWM 都是整个进程的，所以这里不用 t.Parallel，
// 每个形状单独一个子进程。子进程在第一次打开数据库之前打开 SQLITE_CONFIG_MEMSTATUS，
// 并在测量前把 highwater 清零。
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
	enableSQLiteMemStatus(t)
	resetMemHighwater()
	probeBypassCase(t, id)
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
	assertMemStatusWorks(t, tool)
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
	resetMemHighwater()
	res, err := tool.Execute(t.Context(), sqlCall(t, query))
	hw := memHighwater()
	t.Logf("highwater=%d err=%v", hw, err)
	var te *tools.Error
	if !errors.As(err, &te) || te.Code != CodeRejected || te.Retryable || res.Content != "" {
		t.Fatalf("err=%v res=%+v", err, res)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("错误里没有 %q: %v", want, err)
	}
	const sqliteBound = MaxResultColumns*MaxValueBytes + 8<<20
	if hw > sqliteBound {
		t.Fatalf("sqlite3_memory_highwater = %d，大约 128 MiB 的上限是 %d", hw, sqliteBound)
	}
}

func probeSortMemory(t *testing.T) {
	t.Helper()
	ctx := t.Context()
	sortPath := filepath.Join(t.TempDir(), "analytics.db")
	createEmptyDB(t, sortPath)
	fillSortRows(t, sortPath)
	sortDB, err := Open(ctx, sortPath, sortPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sortDB.Close() })
	sortTool, err := NewTool(sortDB, sortPath, sortPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertMemStatusWorks(t, sortTool)
	dir := filepath.Dir(sortPath)
	before, err := listDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	resetMemHighwater()
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
	res, err := sortTool.Execute(ctx, sqlCall(t, `SELECT body FROM t ORDER BY random()`))
	close(stop)
	<-done
	hw := memHighwater()
	t.Logf("highwater=%d err=%v", hw, err)
	if got := currentHeapLimit(); got != 0 {
		t.Fatalf("进程级硬堆上限 = %d，不应设置", got)
	}
	var te *tools.Error
	if hw > SortMemBytes {
		if !errors.As(err, &te) || te.Code != CodeResultTooLarge || te.Retryable || res.Content != "" {
			t.Fatalf("高水位 %d 超过大约 256 MiB，错误应是 sql_result_too_large，实际 err=%v res=%+v", hw, err, res)
		}
		t.Fatalf("sqlite3_memory_highwater = %d，大约 256 MiB 的上限是 %d", hw, SortMemBytes)
	}
	if !errors.As(err, &te) || te.Code != CodeResultTooLarge || te.Retryable || res.Content != "" {
		t.Fatalf("排序 err=%v res=%+v", err, res)
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

// assertMemStatusWorks 先做一次小分配，确认 highwater 真的在计数。
// 计数没打开时 highwater 一直是 0，后面的上限检查会空过。
func assertMemStatusWorks(t *testing.T, tool *Tool) {
	t.Helper()
	resetMemHighwater()
	if _, err := tool.Execute(t.Context(), sqlCall(t, `SELECT length(randomblob(200000))`)); err != nil {
		t.Fatal(err)
	}
	if hw := memHighwater(); hw < 200000 {
		t.Fatalf("sqlite3_memory_highwater = %d，内存统计没有打开", hw)
	}
}

// spawnMemChild 在干净的子进程里跑一个内存用例。
// sqlite3_memory_highwater 和 VmHWM 都看整个进程，不能和别的测试挤在一起，也不能 t.Parallel。
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

func enableSQLiteMemStatus(t *testing.T) {
	t.Helper()
	tls := libc.NewTLS()
	defer tls.Close()
	slot := libc.Xmalloc(tls, 8)
	if slot == 0 {
		t.Fatal("分配参数槽失败")
	}
	defer libc.Xfree(tls, slot)
	libc.VaList(slot, int32(1))
	if rc := sqlite3.Xsqlite3_config(tls, sqlite3.SQLITE_CONFIG_MEMSTATUS, slot); rc != sqlite3.SQLITE_OK {
		t.Fatalf("SQLITE_CONFIG_MEMSTATUS = %d", rc)
	}
}

func resetMemHighwater() {
	tls := libc.NewTLS()
	defer tls.Close()
	sqlite3.Xsqlite3_memory_highwater(tls, 1)
}

func memHighwater() int64 {
	tls := libc.NewTLS()
	defer tls.Close()
	return int64(sqlite3.Xsqlite3_memory_highwater(tls, 0))
}

func currentHeapLimit() int64 {
	tls := libc.NewTLS()
	defer tls.Close()
	return int64(sqlite3.Xsqlite3_hard_heap_limit64(tls, -1))
}
