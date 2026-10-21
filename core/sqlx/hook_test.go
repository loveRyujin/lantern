package sqlx

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/loveRyujin/lantern/core/tools"
	"modernc.org/sqlite"
)

// 文件在查询返回后被改掉时，Execute 必须报哈希不一致。
// 去掉执行后的核对，这个测试会变成成功返回。
func TestExecuteDetectsFileChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "analytics.db")
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
	tool.setAfterQuery(func() {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Errorf("追加字节: %v", err)
			return
		}
		_, _ = f.Write([]byte{0x1})
		_ = f.Close()
	})
	res, err := tool.Execute(ctx, tools.Call{Args: []byte(`{"sql":"SELECT 1"}`)})
	var te *tools.Error
	if !errors.As(err, &te) || te.Code != CodeBusy || !te.Retryable {
		t.Fatalf("err = %v，想要可重试的 data_busy", err)
	}
	if res.Content != "" {
		t.Fatalf("文件变了还返回了结果: %s", res.Content)
	}
	if strings.Contains(err.Error(), "analytics.db") || strings.Contains(err.Error(), filepath.Dir(path)) {
		t.Fatalf("哈希错误带了路径: %v", err)
	}
}

func TestNoMemMapsToRowTooLarge(t *testing.T) {
	if !rowTooLarge(errors.New("out of memory")) {
		t.Fatal("out of memory 没有当成结果过大")
	}
	if !rowTooLarge(errors.New("SQLITE_NOMEM")) {
		t.Fatal("SQLITE_NOMEM 没有当成结果过大")
	}
}

func TestRunSQLBlobCountsBase64(t *testing.T) {
	fit := (MaxResultBytes / 4) * 3
	if base64.StdEncoding.EncodedLen(fit) != MaxResultBytes || base64.StdEncoding.EncodedLen(fit+1) <= MaxResultBytes {
		t.Fatalf("base64 边界不对: fit=%d", fit)
	}
	if fit+1 > MaxResultBytes {
		t.Fatal("原始字节已经超过 1 MiB，测不到 base64 记账")
	}
	tool := openEmptyTool(t)
	ctx := t.Context()

	t.Run("small", func(t *testing.T) {
		raw := []byte{0xff, 0x00, 0xfe}
		res, err := tool.Execute(ctx, tools.Call{Args: []byte(`{"sql":"SELECT x'ff00fe'"}`)})
		if err != nil {
			t.Fatal(err)
		}
		got := firstCellRaw(t, res.Content)
		want := `"` + base64.StdEncoding.EncodeToString(raw) + `"`
		if got != want {
			t.Fatalf("单元格 = %s，想要 %s", got, want)
		}
	})
	t.Run("base64-fits", func(t *testing.T) {
		res, err := tool.Execute(ctx, tools.Call{Args: []byte(fmt.Sprintf(`{"sql":"SELECT zeroblob(%d)"}`, fit))})
		if err != nil {
			t.Fatal(err)
		}
		enc := strings.Trim(firstCellRaw(t, res.Content), `"`)
		if len(enc) != MaxResultBytes {
			t.Fatalf("base64 长度 = %d，想要 %d", len(enc), MaxResultBytes)
		}
		body, err := base64.StdEncoding.DecodeString(enc)
		if err != nil {
			t.Fatal(err)
		}
		if len(body) != fit {
			t.Fatalf("解出来 %d 字节，想要 %d", len(body), fit)
		}
	})
	t.Run("base64-over-cap", func(t *testing.T) {
		res, err := tool.Execute(ctx, tools.Call{Args: []byte(fmt.Sprintf(`{"sql":"SELECT zeroblob(%d)"}`, fit+1))})
		var te *tools.Error
		if !errors.As(err, &te) || te.Code != CodeResultTooLarge || te.Retryable || res.Content != "" {
			t.Fatalf("err=%v res=%+v", err, res)
		}
	})
}

func openEmptyTool(t *testing.T) *Tool {
	t.Helper()
	path := filepath.Join(t.TempDir(), "analytics.db")
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

func firstCellRaw(t *testing.T, content string) string {
	t.Helper()
	var parsed struct {
		Rows      [][]json.RawMessage `json:"rows"`
		RowCount  int                 `json:"row_count"`
		Truncated bool                `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		t.Fatalf("解析结果: %v body=%s", err, content)
	}
	if parsed.RowCount != 1 || parsed.Truncated || len(parsed.Rows) != 1 || len(parsed.Rows[0]) != 1 {
		t.Fatalf("结果形状不对: %s", content)
	}
	return string(parsed.Rows[0][0])
}

func TestWideRowMemoryBound(t *testing.T) {
	const id = "wide-row"
	if os.Getenv("LANTERN_SQL_MEM_CHILD") != id {
		if os.Getenv("LANTERN_SQL_MEM_CHILD") != "" {
			t.Skip("留给别的内存子进程")
		}
		// VmHWM 是整个进程的高水位，清不掉。单独一个子进程，别的测试的峰值才进不来。
		spawnMemChild(t, "TestWideRowMemoryBound", id)
		return
	}
	enableSQLiteMemStatus(t)
	resetMemHighwater()
	path := filepath.Join(t.TempDir(), "analytics.db")
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
	const blob = 3 << 20
	parts := make([]string, MaxResultColumns)
	for i := range parts {
		parts[i] = "randomblob(3145728)"
	}
	resetMemHighwater()
	before := processHWM(t)
	var live int64
	onSQLiteRow = func() {
		if n := mustStatusKB(t, "VmHWM:"); n > live {
			live = n
		}
	}
	t.Cleanup(func() { onSQLiteRow = nil })
	res, err := tool.Execute(ctx, tools.Call{Args: []byte(`{"sql":"SELECT ` + strings.Join(parts, ", ") + `"}`)})
	if n := processHWM(t); n > live {
		live = n
	}
	var te *tools.Error
	if !errors.As(err, &te) || te.Code != CodeResultTooLarge || te.Retryable || res.Content != "" {
		t.Fatalf("宽行 err=%v res=%+v", err, res)
	}
	// 这个子进程在打开数据库之前打开了 MEMSTATUS，并清过 sqlite3_memory_highwater。
	// 宽行仍读 VmHWM：modernc 的 malloc 在 libc 堆上，Go 的 TotalAlloc 看不到。
	// VmHWM 清不掉，所以这个用例不能和别的测试待在同一个进程里。
	delta := live - before
	const sqliteBound = MaxResultColumns * MaxValueBytes
	t.Logf("VmHWM 增加 %d 字节", delta)
	if delta < MaxResultColumns*blob-4<<20 || delta > sqliteBound+32<<20 {
		t.Fatalf("这一行 VmHWM 增加 %d，应盖住 %d 且不超过大约 128 MiB（%d）", delta, MaxResultColumns*blob, sqliteBound)
	}
}

func processHWM(t *testing.T) int64 {
	t.Helper()
	return mustStatusKB(t, "VmHWM:")
}

func mustStatusKB(t *testing.T, key string) int64 {
	t.Helper()
	body, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatalf("读 %s: %v", key, err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(line, key) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			break
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		return kb << 10
	}
	t.Fatalf("没有 %s", key)
	return 0
}

func createEmptyDB(t *testing.T, path string) {
	t.Helper()
	u := url.URL{
		Scheme:   "file",
		Path:     filepath.ToSlash(path),
		RawQuery: url.Values{"_journal_mode": {"DELETE"}}.Encode(),
	}
	drv := &sqlite.Driver{}
	db := sql.OpenDB(hookConnector{dsn: u.String(), drv: drv})
	defer db.Close()
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `CREATE TABLE t (id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
	_ = os.Remove(path + "-journal")
}

type hookConnector struct {
	dsn string
	drv *sqlite.Driver
}

func (c hookConnector) Connect(ctx context.Context) (driver.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.drv.Open(c.dsn)
}

func (c hookConnector) Driver() driver.Driver { return c.drv }
