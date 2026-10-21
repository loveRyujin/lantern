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
	"strings"
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

// Open 用 mode=ro 打开 path 指向的业务库，并在每条连接（含连接池重建）上打开 query_only、拒绝 ATTACH。
// path 会先做 EvalSymlinks，打开的是解析后的那个文件。文件不存在时不会创建。
// 敏感库不在这条路径上：路径、符号链接指向的目标、URI 参数都不能把连接指到 sensitive.db。
func Open(ctx context.Context, path string) (*sql.DB, error) {
	if ctx == nil {
		return nil, errors.New("sqlx: ctx 不能为 nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := rejectSensitiveTarget(path); err != nil {
		return nil, err
	}
	resolved, err := Resolve(path)
	if err != nil {
		return nil, err
	}
	if err := rejectSensitiveTarget(resolved); err != nil {
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
	// 记下打开时有没有 -wal。之后才出现的 -wal 说明写入者改了日志模式，
	// 这条连接上的旧快照不能再交给模型。
	snap, err := Snapshot(resolved)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	rememberGeneration(resolved, snap)
	return db, nil
}

// readOnlyDSN 用 file: URI。mode=ro 由 SQLite 解释，_query_only 由驱动在每条连接上执行。
// 不设置 journal_mode：那是写操作，只读打开不该改日志模式。
// 不加 immutable。导入、种子或迁移写入时拿着锁；读不到这把锁就返回 data_busy，
// 不把可能过期的行交给模型。busy_timeout 到点仍拿不到锁，也是同一种结果。
func readOnlyDSN(path string) (string, error) {
	q := url.Values{
		"mode":          {"ro"},
		"_busy_timeout": {"5000"},
		"_query_only":   {"on"},
	}
	u := url.URL{
		Scheme:   "file",
		Path:     filepath.ToSlash(path),
		RawQuery: q.Encode(),
	}
	return u.String(), nil
}

// rejectSensitiveTarget 拒绝把只读连接指到敏感库。
// URI（file: 或查询参数）也不接受：那是换库或换打开方式的入口，业务库只用文件路径。
func rejectSensitiveTarget(path string) error {
	if path == "" {
		return errors.New("sqlx: 分析库路径为空")
	}
	trimmed := strings.TrimSpace(path)
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "file:") || strings.ContainsAny(trimmed, "?\x00") {
		return errors.New("sqlx: 只打开业务库文件")
	}
	if strings.EqualFold(filepath.Base(path), "sensitive.db") {
		return errors.New("sqlx: 不能打开敏感库")
	}
	return nil
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
	if err := limitResultLength(conn); err != nil {
		return err
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

// generation 是某次 Open 时的文件状态。之后才出现的 -wal 不能沿用当时的连接。
type generation struct {
	mu   sync.Mutex
	snap Snap
}

var openGens sync.Map

func rememberGeneration(resolved string, snap Snap) {
	openGens.Store(resolved, &generation{snap: snap})
}

func hasWAL(snap Snap) bool {
	for _, side := range snap.Sides {
		if side.Exists {
			return true
		}
	}
	return false
}

// claimNewWAL 在打开之后才出现 -wal 时返回 true，并把这一代记成当前文件。
// 调用方丢掉旧连接，这次查询返回 data_busy，下一次再用新连接读。
func claimNewWAL(current Snap) bool {
	v, ok := openGens.Load(current.Resolved)
	if !ok {
		return false
	}
	g := v.(*generation)
	g.mu.Lock()
	defer g.mu.Unlock()
	if hasWAL(g.snap) || !hasWAL(current) {
		return false
	}
	g.snap = current
	return true
}

// adoptGeneration 在查询过程中新出现 -wal 时更新这一代，让下一次重试打开新连接。
func adoptGeneration(current Snap) {
	v, ok := openGens.Load(current.Resolved)
	if !ok {
		return
	}
	g := v.(*generation)
	g.mu.Lock()
	g.snap = current
	g.mu.Unlock()
}

// dropIdle 关掉池里闲置的连接。Open 之后日志模式变了，那些连接还停在旧快照上。
func dropIdle(db Queryer) {
	sqlDB, ok := db.(*sql.DB)
	if !ok {
		return
	}
	sqlDB.SetMaxIdleConns(0)
	sqlDB.SetMaxIdleConns(2)
}
