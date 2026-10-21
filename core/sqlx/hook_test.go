package sqlx

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"errors"
	"net/url"
	"os"
	"path/filepath"
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

func TestCellBytesCountsBase64(t *testing.T) {
	raw := []byte{0xff, 0x00, 0xfe}
	if got, want := cellBytes(raw), base64.StdEncoding.EncodedLen(len(raw)); got != want {
		t.Fatalf("[]byte 按原始长度计了 %d，编码后是 %d", got, want)
	}
	if MaxValueBytes <= MaxResultBytes {
		t.Fatalf("中间值上限 %d 没有比结果行 %d 松", MaxValueBytes, MaxResultBytes)
	}
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
