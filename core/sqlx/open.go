package sqlx

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"modernc.org/sqlite"
)

// 只读池和可写的工作区池不是同一个 Driver。工作区池要跑迁移，不能打开 query_only。
var (
	roOnce sync.Once
	roDrv  *sqlite.Driver
)

func readOnlyDriver() *sqlite.Driver {
	roOnce.Do(func() {
		roDrv = &sqlite.Driver{}
		roDrv.RegisterConnectionHook(func(conn sqlite.ExecQuerierContext, _ string) error {
			return installReadOnly(conn)
		})
	})
	return roDrv
}

type roConnector struct {
	dsn string
	drv *sqlite.Driver
}

func (c roConnector) Connect(ctx context.Context) (driver.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.drv.Open(c.dsn)
}

func (c roConnector) Driver() driver.Driver { return c.drv }

// Open 用 mode=ro 打开 path 指向的库，并在每条连接（含连接池重建）上打开 query_only、拒绝 ATTACH。
// path 会先做 EvalSymlinks，打开的是解析后的那个文件。文件不存在时不会创建。
func Open(ctx context.Context, path string) (*sql.DB, error) {
	if ctx == nil {
		return nil, errors.New("sqlx: ctx 不能为 nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	resolved, err := Resolve(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("sqlx: 打开分析库: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("sqlx: 分析库不是普通文件")
	}
	dsn, err := readOnlyDSN(resolved)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(roConnector{dsn: dsn, drv: readOnlyDriver()})
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlx: 打开分析库: %w", err)
	}
	return db, nil
}

// readOnlyDSN 用 file: URI。mode=ro 由 SQLite 解释，_query_only 由驱动在每条连接上执行。
// 不设置 journal_mode：那是写操作，只读打开不该改日志模式。
// 旁边没有 -wal 时加上 immutable。WAL 模式的库在普通只读打开时会把 -wal 建出来，
// 而文件哈希要求执行前没有 -wal 时执行后也不能有。已有 -wal 时不能加 immutable，
// 否则读不到还没 checkpoint 的提交。
func readOnlyDSN(path string) (string, error) {
	q := url.Values{
		"mode":          {"ro"},
		"_busy_timeout": {"5000"},
		"_query_only":   {"on"},
	}
	_, err := os.Stat(path + "-wal")
	if err != nil {
		if !os.IsNotExist(err) {
			return "", errors.New("sqlx: 无法确认分析库的 -wal")
		}
		// 没有 -wal 说明提交已经在主库里。immutable 让这次打开不去新建 -wal。
		q.Set("immutable", "1")
	}
	u := url.URL{
		Scheme:   "file",
		Path:     filepath.ToSlash(path),
		RawQuery: q.Encode(),
	}
	return u.String(), nil
}

type mainReadOnly interface {
	IsReadOnly(schema string) (bool, error)
}

// installReadOnly 在驱动把连接交给连接池之前跑。钩子对每条新连接执行一次，回收重建也不例外。
func installReadOnly(conn sqlite.ExecQuerierContext) error {
	if err := RejectAttach(conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(context.Background(), "PRAGMA query_only = ON", nil); err != nil {
		return fmt.Errorf("sqlx: 设置 query_only: %w", err)
	}
	on, err := readQueryOnly(conn)
	if err != nil {
		return err
	}
	if on != 1 {
		return fmt.Errorf("sqlx: query_only = %d，想要 1", on)
	}
	checker, ok := conn.(mainReadOnly)
	if !ok {
		return errors.New("sqlx: 连接无法确认只读，拒绝打开")
	}
	yes, err := checker.IsReadOnly("main")
	if err != nil {
		return fmt.Errorf("sqlx: 确认只读: %w", err)
	}
	if !yes {
		return errors.New("sqlx: 连接不是以只读方式打开，拒绝使用")
	}
	// query_only 已经打开。之后再拦“把它关掉”和 writable_schema，避免连接回到池里时标志被改掉。
	if err := lockReadOnly(conn); err != nil {
		return err
	}
	return nil
}

func readQueryOnly(conn sqlite.ExecQuerierContext) (int64, error) {
	rows, err := conn.QueryContext(context.Background(), "PRAGMA query_only", nil)
	if err != nil {
		return 0, fmt.Errorf("sqlx: 读取 query_only: %w", err)
	}
	defer rows.Close()
	dest := make([]driver.Value, len(rows.Columns()))
	if err := rows.Next(dest); err != nil {
		return 0, fmt.Errorf("sqlx: 读取 query_only: %w", err)
	}
	n, err := valueInt(dest[0])
	if err != nil {
		return 0, err
	}
	return n, nil
}

func valueInt(v driver.Value) (int64, error) {
	switch n := v.(type) {
	case int64:
		return n, nil
	case int32:
		return int64(n), nil
	case int:
		return int64(n), nil
	case string:
		return strconv.ParseInt(n, 10, 64)
	case []byte:
		return strconv.ParseInt(string(n), 10, 64)
	default:
		return 0, fmt.Errorf("sqlx: query_only 的类型是 %T", v)
	}
}
