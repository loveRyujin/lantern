package workspace

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net/url"
	"path/filepath"
	"reflect"
	"sync"
	"unsafe"

	"modernc.org/libc"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// 技术设计 §14：analytics 连接要设 SQLITE_LIMIT_ATTACHED=0，并用 authorizer 拒绝
// SQLITE_ATTACH。modernc 的 Driver 文档写明它不暴露 authorizer；sqlite.Limit 又要
// 已经包进 database/sql 的 *sql.Conn，连接钩子里还拿不到。钩子拿到的是驱动自己的
// 连接，这里在交还给连接池之前直接调用 lib 里的 sqlite3_limit 和 sqlite3_set_authorizer。
// 读不到连接内部字段就拒绝打开，避免静默退回到没有防护的连接。
//
// 四个库走同一个驱动，所以 kb、sensitive、runs 也拒绝 ATTACH。外部 SQL 只会落到
// analytics 上，这是验收要挡住的那条连接。

var (
	driverOnce sync.Once
	drv        *sqlite.Driver
)

func workspaceDriver() *sqlite.Driver {
	driverOnce.Do(func() {
		drv = &sqlite.Driver{}
		drv.RegisterConnectionHook(func(conn sqlite.ExecQuerierContext, _ string) error {
			return installAttachGuard(conn)
		})
	})
	return drv
}

type dsnConnector struct {
	dsn string
	drv *sqlite.Driver
}

func (c dsnConnector) Connect(ctx context.Context) (driver.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.drv.Open(c.dsn)
}

func (c dsnConnector) Driver() driver.Driver { return c.drv }

func openAll(ctx context.Context, id, root, dir string) (*Handle, error) {
	h := &Handle{ID: id, Root: root, Dir: dir}
	var err error
	defer func() {
		if err != nil {
			_ = h.Close()
		}
	}()

	h.KB, err = openDB(ctx, filepath.Join(dir, fileKB), kbMigrations, "migrations/kb", dropVectorsNew)
	if err != nil {
		return nil, fmt.Errorf("workspace: kb: %w", err)
	}
	h.Analytics, err = openDB(ctx, filepath.Join(dir, fileAnalytics), analyticsMigrations, "migrations/analytics", nil)
	if err != nil {
		return nil, fmt.Errorf("workspace: analytics: %w", err)
	}
	h.Sensitive, err = openDB(ctx, filepath.Join(dir, fileSensitive), sensitiveMigrations, "migrations/sensitive", nil)
	if err != nil {
		return nil, fmt.Errorf("workspace: sensitive: %w", err)
	}
	h.Runs, err = openDB(ctx, filepath.Join(dir, fileRuns), runsMigrations, "migrations/runs", nil)
	if err != nil {
		return nil, fmt.Errorf("workspace: runs: %w", err)
	}
	return h, nil
}

func openDB(ctx context.Context, path string, migrations migrationFS, migDir string, after func(context.Context, *sql.DB) error) (*sql.DB, error) {
	db := sql.OpenDB(dsnConnector{dsn: sqliteDSN(path), drv: workspaceDriver()})
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migrateDir(ctx, db, migrations, migDir); err != nil {
		_ = db.Close()
		return nil, err
	}
	if after != nil {
		if err := after(ctx, db); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return db, nil
}

// sqliteDSN 用 file: URI 打开，并在每条连接上打开外键和 WAL。
func sqliteDSN(path string) string {
	u := url.URL{
		Scheme: "file",
		Path:   filepath.ToSlash(path),
		RawQuery: url.Values{
			"_busy_timeout": {"5000"},
			"_foreign_keys": {"on"},
			"_journal_mode": {"WAL"},
		}.Encode(),
	}
	return u.String()
}

func installAttachGuard(conn sqlite.ExecQuerierContext) error {
	db, tls, err := sqliteConn(conn)
	if err != nil {
		return err
	}
	if got := sqlite3.Xsqlite3_limit(tls, db, sqlite3.SQLITE_LIMIT_ATTACHED, 0); got < 0 {
		return fmt.Errorf("workspace: sqlite3_limit(SQLITE_LIMIT_ATTACHED): %d", got)
	}
	if got := sqlite3.Xsqlite3_limit(tls, db, sqlite3.SQLITE_LIMIT_ATTACHED, -1); got != 0 {
		return fmt.Errorf("workspace: SQLITE_LIMIT_ATTACHED = %d, want 0", got)
	}
	rc := sqlite3.Xsqlite3_set_authorizer(tls, db, cFuncPointer(denyAttach), 0)
	if rc != sqlite3.SQLITE_OK {
		return fmt.Errorf("workspace: sqlite3_set_authorizer: %d", rc)
	}
	return nil
}

// denyAttach 是 sqlite3_set_authorizer 的回调。必须是包级函数：modernc 用函数值的
// 代码指针注册回调，闭包的表示和这个约定不一致。除 SQLITE_ATTACH 以外全部放行，
// 迁移和业务写入不受影响。
func denyAttach(_ *libc.TLS, _ uintptr, action int32, _, _, _, _ uintptr) int32 {
	if action == sqlite3.SQLITE_ATTACH {
		return sqlite3.SQLITE_DENY
	}
	return sqlite3.SQLITE_OK
}

// cFuncPointer 与 modernc.org/sqlite 内部把 Go 函数交给 ccgo 的方式相同。
func cFuncPointer[T any](f T) uintptr {
	return *(*uintptr)(unsafe.Pointer(&struct{ f T }{f}))
}

func sqliteConn(conn sqlite.ExecQuerierContext) (uintptr, *libc.TLS, error) {
	v := reflect.ValueOf(conn)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return 0, nil, fmt.Errorf("workspace: sqlite connection type %T has no handle", conn)
	}
	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return 0, nil, fmt.Errorf("workspace: sqlite connection type %T has no handle", conn)
	}
	dbField := v.FieldByName("db")
	tlsField := v.FieldByName("tls")
	if !dbField.IsValid() || !tlsField.IsValid() {
		return 0, nil, fmt.Errorf("workspace: sqlite connection type %T has no db/tls fields; refusing to open without ATTACH guard", conn)
	}
	dbField = exportedView(dbField)
	tlsField = exportedView(tlsField)
	db := uintptr(dbField.Uint())
	tls, ok := tlsField.Interface().(*libc.TLS)
	if !ok || tls == nil || db == 0 {
		return 0, nil, fmt.Errorf("workspace: sqlite connection type %T has an empty handle", conn)
	}
	return db, tls, nil
}

// exportedView 读取其他包里的未导出字段。modernc v1.60.1 的 *conn.db / *conn.tls 没有导出访问器。
func exportedView(f reflect.Value) reflect.Value {
	if f.CanInterface() {
		return f
	}
	return reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem()
}
