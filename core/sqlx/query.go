package sqlx

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
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
	// MaxResultBytes 是交回模型的一行最多能有的字节。中间值不算在这里。
	MaxResultBytes = 1 << 20
	// MaxValueBytes 是 SQLite 里单个值和表达式中间值的上限。
	// 2 MiB 的中间值还能返回 length()。不按列数再除，也不用 SQLITE_LIMIT_COLUMN。
	MaxValueBytes = 4 << 20
	// MaxResultColumns 是最终结果最多能有的列数。准备语句之后、第一步之前检查。
	// 超过就拒绝，让模型改写。不写进 SQLITE_LIMIT_COLUMN，否则宽表会让整个库打不开。
	MaxResultColumns = 32
	TruncateBytes    = "bytes"
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

// Query 执行一条已经通过扫描的语句，最多取 limit 行，并且不超过 MaxResultBytes。
// 多读一行只为了判断要不要标 truncated。limit 必须在 1 和 MaxLimit 之间。
// 准备完成就先看结果列数，超过 [MaxResultColumns] 返回 [errTooManyColumns]，此时还没有 step。
// 每一行按 sqlite3_column_bytes 逐列累加，拷贝之前就停。一行超过 MaxResultBytes，或者 SQLite 报 TOOBIG，
// 返回 [errRowTooLarge]，这次已读出的行也不返回。
// 多行各自没超限、加在一起超了，才截断并保留已经放下的完整行。
// 表达式的中间值不受这 1 MiB 约束，只受连接上的 MaxValueBytes 约束。
// SQLite 在返回一行之前会把每一列都算进寄存器，所以单值 4 MiB、结果 32 列时，SQLite 侧最坏大约 128 MiB。
// Go 这边停在 1 MiB，不再把超出的列拷进来。
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

func cellBytes(v any) int {
	switch x := v.(type) {
	case nil:
		return 0
	case string:
		return len(x)
	case []byte:
		// JSON 把 []byte 编成 base64。按模型看到的长度计，不按原始字节数。
		return base64.StdEncoding.EncodedLen(len(x))
	case int64:
		return len(strconv.FormatInt(x, 10))
	case float64:
		return len(strconv.FormatFloat(x, 'f', -1, 64))
	case bool:
		if x {
			return 4
		}
		return 5
	default:
		return len(fmt.Sprint(x))
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
func rowTooLarge(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errRowTooLarge) || isTooBig(err) {
		return true
	}
	return strings.Contains(err.Error(), errRowTooLarge.Error())
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
