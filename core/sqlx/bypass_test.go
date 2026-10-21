package sqlx

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/loveRyujin/lantern/core/tools"
	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// TestRunSQLBypassShapesStayBounded 把改写器的三条绕过收成回归。
// CTE 后面的 SELECT *、多表 FROM 里的子查询、以及用户自己写的 lantern_cell，
// 都不能把峰值内存撑过大约 128 MiB（SQLite）加 1 MiB（Go）。
// modernc 默认关掉内存统计，sqlite3_memory_highwater 会一直是 0。
// 子进程在第一次打开数据库之前打开 SQLITE_CONFIG_MEMSTATUS，计数才有效。
func TestRunSQLBypassShapesStayBounded(t *testing.T) {
	if os.Getenv("LANTERN_SQL_MEM_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestRunSQLBypassShapesStayBounded$", "-test.count=1", "-test.v")
		cmd.Env = append(os.Environ(), "LANTERN_SQL_MEM_CHILD=1")
		out, err := cmd.CombinedOutput()
		t.Log(string(out))
		if err != nil {
			t.Fatalf("绕过形状的内存子进程失败: %v", err)
		}
		return
	}
	probeBypassMemory(t)
}

func probeBypassMemory(t *testing.T) {
	t.Helper()
	enableSQLiteMemStatus(t)
	path := t.TempDir() + "/analytics.db"
	createEmptyDB(t, path)
	ctx := t.Context()
	db, err := Open(ctx, path, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tool, err := NewTool(db, path, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resetMemHighwater()
	if _, err := tool.Execute(ctx, sqlCall(t, `SELECT length(randomblob(200000))`)); err != nil {
		t.Fatal(err)
	}
	if hw := memHighwater(); hw < 200000 {
		t.Fatalf("sqlite3_memory_highwater = %d，内存统计没有打开", hw)
	}

	cols := wideExprs(40, "hex(zeroblob(2000000))")
	cases := []struct {
		name  string
		query string
		want  string
	}{
		{
			name:  "cte-star",
			query: "WITH x AS (SELECT " + cols + ") SELECT * FROM x",
			want:  "32",
		},
		{
			name:  "join-subquery",
			query: "SELECT * FROM (SELECT 1 AS k) a, (SELECT " + cols + ") b",
			want:  "32",
		},
		{
			name:  "lantern-cell",
			query: "SELECT " + wideExprs(40, "lantern_cell(0, hex(zeroblob(2000000)))"),
			want:  "lantern_cell",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetMemHighwater()
			runtime.GC()
			var before runtime.MemStats
			runtime.ReadMemStats(&before)
			res, err := tool.Execute(ctx, sqlCall(t, tc.query))
			hw := memHighwater()
			var after runtime.MemStats
			runtime.ReadMemStats(&after)
			heap := after.TotalAlloc - before.TotalAlloc
			t.Logf("highwater=%d heap=%d err=%v", hw, heap, err)
			var te *tools.Error
			if !errors.As(err, &te) || te.Code != CodeRejected || te.Retryable || res.Content != "" {
				t.Fatalf("err=%v res=%+v", err, res)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误里没有 %q: %v", tc.want, err)
			}
			const sqliteBound = MaxResultColumns*MaxValueBytes + 8<<20
			if hw > sqliteBound {
				t.Fatalf("sqlite3_memory_highwater = %d，大约 128 MiB 的上限是 %d", hw, sqliteBound)
			}
			if heap > MaxResultBytes {
				t.Fatalf("Go 分配了 %d 字节，上限 %d", heap, MaxResultBytes)
			}
		})
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
