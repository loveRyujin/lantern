package sqlx

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/loveRyujin/lantern/core/errcode"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Queryer 是只读查询用的连接池。*sql.DB 满足它。
type Queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// 结果字节上限。和行数上限一起生效。单个单元格也算进这个总数。
const (
	// MaxResultBytes 是交回模型的完整 JSON 上限（含列名、外壳、转义和 base64）。
	MaxResultBytes = 1 << 20
	// MaxValueBytes 是 SQLite 里单个值和表达式中间值的上限。
	// 2 MiB 的中间值还能返回 length()。不按列数再除，也不用 SQLITE_LIMIT_COLUMN。
	MaxValueBytes = 4 << 20
	// MaxResultColumns 是最终结果最多能有的列数。准备语句之后、第一步之前检查。
	// 超过就拒绝，让模型改写。不写进 SQLITE_LIMIT_COLUMN，否则宽表会让整个库打不开。
	MaxResultColumns = 32
	// CacheKiB 是只读连接的页缓存，单位 KiB。PRAGMA cache_size 用负数表示 KiB。
	// 留小，只让 ORDER BY 的排序把多出来的行写进临时文件。
	// 子查询、物化 CTE、GROUP BY、UNION 和 IN 列表不受 temp_store=FILE 和小缓存约束。
	// 不设置 sqlite3_hard_heap_limit64：那个上限是进程级的，工作区、审计和敏感库也会被卡住。
	CacheKiB = 256
	// SortMemBytes 是排序这类查询在 SQLite 堆上的大约上限，只给回归测试用。
	// 这里只量低层排序器；产品工具另外受 Linux worker 的 RLIMIT_AS 约束。
	SortMemBytes  int64 = 256 << 20
	TruncateBytes       = "bytes"
)

// Result 是截断之后的查询结果。RowCount 是实际返回的行数，不是库里的总行数。
// TruncateReason 为 bytes 时，是总字节上限把结果截断了。
type Result struct {
	Columns        []string `json:"columns"`
	Rows           [][]any  `json:"rows"`
	RowCount       int      `json:"row_count"`
	Truncated      bool     `json:"truncated"`
	TruncateReason string   `json:"truncate_reason,omitempty"`
}

// EncodedText 保留 SQLite TEXT 里的非法 UTF-8；不会把原始字节替换成 U+FFFD。
// 合法 TEXT 仍返回字符串，BLOB 仍按 encoding/json 的 []byte 约定返回 base64 字符串。
type EncodedText struct {
	Encoding string `json:"encoding"`
	Data     string `json:"data"`
}

// Query 是受信任调用方使用的低层读取器，直接使用当前进程的 SQLite 连接。
// 模型工具必须经 NewTool，不能直接调用本函数；Linux Tool 在设有 RLIMIT_AS 的 worker 内使用它。
// limit 必须在 1 和 MaxLimit 之间；多读一行判断是否截断。结果列在 prepare 后最多 32 列。
// 每列先拒绝明显超限的原始数据，再按实际 JSON 长度累计，包含字符串转义和 base64。
// 单行超限或 SQLite TOOBIG/NOMEM 时整次失败；多行累计超限时只保留完整行并标截断。
// 最终 JSON 含列名和元数据最多 1 MiB，编码时关闭 HTML 转义。非法 UTF-8 TEXT 用 EncodedText 无损表示。
// 本函数不限制 SQLite 中间值总内存，不能代替 Tool 的进程隔离。
func Query(ctx context.Context, db Queryer, query string, limit int) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, &codedTimeout{err: err}
	}
	if db == nil {
		return Result{}, errors.New("sqlx: 没有数据库")
	}
	if limit < 1 || limit > MaxLimit {
		return Result{}, errors.New("sqlx: 行数上限不在允许范围内")
	}
	return queryRows(ctx, db, query, limit)
}

type connOpener interface {
	Conn(context.Context) (*sql.Conn, error)
}

func queryRows(ctx context.Context, db Queryer, query string, limit int) (Result, error) {
	opener, ok := db.(connOpener)
	if !ok {
		return Result{}, errors.New("sqlx: 连接不能准备语句")
	}
	conn, err := opener.Conn(ctx)
	if err != nil {
		if isTimeout(ctx, err) {
			return Result{}, &codedTimeout{err: err}
		}
		return Result{}, err
	}
	defer conn.Close()
	var result Result
	var qerr error
	rawErr := conn.Raw(func(driverConn any) error {
		result, qerr = stepQuery(ctx, driverConn, query, limit)
		return nil
	})
	if rawErr != nil {
		if isTimeout(ctx, rawErr) {
			return Result{}, &codedTimeout{err: rawErr}
		}
		return Result{}, rawErr
	}
	return result, qerr
}

// errRowTooLarge 表示单独一行放不进字节上限。调用方丢掉这次已经读出的行。
// 覆盖两条路：拷贝前按列累计超过 MaxResultBytes，以及 SQLite 报 TOOBIG。
var errRowTooLarge = errors.New("sqlx: 单行结果超过字节上限")

// errTooManyColumns 表示结果列数超过 MaxResultColumns。调用方把它当成 sql_rejected。
// 这是在第一步之前发现的，语句还没有执行。
var errTooManyColumns = errors.New("sqlx: 结果超过 32 列")

func cutByBytes(cols []string, rows [][]any) Result {
	if cols == nil {
		cols = []string{}
	}
	if rows == nil {
		rows = [][]any{}
	}
	return Result{
		Columns:        cols,
		Rows:           rows,
		RowCount:       len(rows),
		Truncated:      true,
		TruncateReason: TruncateBytes,
	}
}

func isTooBig(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) && se != nil && se.Code()&0xff == sqlite3.SQLITE_TOOBIG {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "sqlite_toobig") || strings.Contains(msg, "too big")
}

// rowTooLarge 是整次查询该失败的结果过大。
// TOOBIG 来自超过 MaxValueBytes 的单个值或中间值。结果行超过 MaxResultBytes 时，
// 列门闩在拷贝这一列之前返回 [errRowTooLarge]，后面的列不再计算。
// NOMEM 也走这里；在 worker 内触及系统地址空间限制后的分配失败同样返回 sql_result_too_large。
func rowTooLarge(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errRowTooLarge) || isTooBig(err) || isNoMem(err) {
		return true
	}
	return strings.Contains(err.Error(), errRowTooLarge.Error())
}

func isNoMem(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) && se != nil {
		code := se.Code()
		if code&0xff == sqlite3.SQLITE_NOMEM || code == sqlite3.SQLITE_IOERR_NOMEM {
			return true
		}
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "out of memory") || strings.Contains(msg, "sqlite_nomem")
}

func isBusy(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) && se != nil {
		code := se.Code() & 0xff
		return code == sqlite3.SQLITE_BUSY || code == sqlite3.SQLITE_LOCKED
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "sqlite_busy") || strings.Contains(msg, "database is locked") || strings.Contains(msg, "sqlite_locked")
}

type codedTimeout struct {
	err error
}

func (e *codedTimeout) Error() string {
	return "查询超时"
}

func (e *codedTimeout) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func (e *codedTimeout) Code() errcode.Code {
	return errcode.Timeout
}

func isTimeout(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return true
	}
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "interrupt")
}
