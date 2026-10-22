package workspace

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net/url"
	"path/filepath"
	"sync"

	"github.com/loveRyujin/lantern/core/sqlx"
	"modernc.org/sqlite"
)

// 技术设计 §14：每条由本包打开的连接都拒绝 ATTACH。实现在 [sqlx.RejectAttach]：
// 设 SQLITE_LIMIT_ATTACHED=0，并用 authorizer 拒绝 SQLITE_ATTACH。读不到连接内部
// 字段就拒绝打开，避免静默退回到没有防护的连接。
//
// 这里的连接要跑迁移和种子写入，所以不开 mode=ro，也不开 query_only。
// 模型提交的 SQL 走 sqlx 的只读连接，不走这个池。
//
// kb、analytics、runs 在启动时打开，sensitive 要等 [Handle.OpenSensitive]。它们走同一个
// 驱动，所以确认导出之后打开的连接也拒绝 ATTACH。

var (
	driverOnce sync.Once
	drv        *sqlite.Driver
)

func workspaceDriver() *sqlite.Driver {
	driverOnce.Do(func() {
		drv = &sqlite.Driver{}
		drv.RegisterConnectionHook(func(conn sqlite.ExecQuerierContext, _ string) error {
			return sqlx.RejectAttach(conn)
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
