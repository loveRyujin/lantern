package sqlx

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/loveRyujin/lantern/core/errcode"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// sqliteMinLength 是 SQLite 允许的 SQLITE_LIMIT_LENGTH 下限。再小会被抬回这个值。
const sqliteMinLength = 30

// Queryer 是只读查询用的连接池。*sql.DB 满足它。
type Queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// 结果字节上限。和行数上限一起生效。单个单元格也算进这个总数。
const (
	MaxResultBytes = 1 << 20
	TruncateBytes  = "bytes"
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
// 不改写 SQL。多读一行只为了判断要不要标 truncated。limit 必须在 1 和 MaxLimit 之间。
// 字节按读到的单元格累计，这一行放不下就停下，不把超限的那一行放进结果。
// 列数多于 1 时，先把 SQLITE_LIMIT_LENGTH 收成剩余预算除以列数，再执行。
// SQLite 在生成超限的值之前就会返回 TOOBIG，整行不会先进入内存。
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
	if pool, ok := db.(interface {
		Conn(context.Context) (*sql.Conn, error)
	}); ok {
		return queryCapped(ctx, pool, query, limit)
	}
	return queryRows(ctx, db, query, limit)
}

func queryCapped(ctx context.Context, pool interface {
	Conn(context.Context) (*sql.Conn, error)
}, query string, limit int) (Result, error) {
	conn, err := pool.Conn(ctx)
	if err != nil {
		if isTimeout(ctx, err) {
			return Result{}, &codedTimeout{err: err}
		}
		return Result{}, err
	}
	defer conn.Close()
	nCols, err := resultColumns(ctx, conn, query)
	if err != nil {
		if isTimeout(ctx, err) {
			return Result{}, &codedTimeout{err: err}
		}
		if isTooBig(err) {
			return cutByBytes(nil, nil), nil
		}
		return Result{}, err
	}
	per := cellLimit(nCols)
	if _, err := sqlite.Limit(conn, sqlite3.SQLITE_LIMIT_LENGTH, per); err != nil {
		return Result{}, err
	}
	defer func() {
		_, _ = sqlite.Limit(conn, sqlite3.SQLITE_LIMIT_LENGTH, MaxResultBytes)
	}()
	return queryRows(ctx, conn, query, limit)
}

// cellLimit 是这一行里单个值允许的最大字节数。
// 各列之和不超过 MaxResultBytes，SQLite 才不会在我们计数之前把整行分配出来。
func cellLimit(nCols int) int {
	if nCols < 1 {
		nCols = 1
	}
	per := MaxResultBytes / nCols
	if per < sqliteMinLength {
		per = sqliteMinLength
	}
	if per > MaxResultBytes {
		return MaxResultBytes
	}
	return per
}

func resultColumns(ctx context.Context, conn *sql.Conn, query string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var n int
	err := conn.Raw(func(dc any) error {
		describer, ok := dc.(interface {
			ColumnInfo(string) ([]sqlite.ColumnInfo, error)
		})
		if !ok {
			return fmt.Errorf("sqlx: 连接无法读取列数 (%T)", dc)
		}
		info, err := describer.ColumnInfo(query)
		if err != nil {
			return err
		}
		n = len(info)
		return nil
	})
	return n, err
}

func queryRows(ctx context.Context, db Queryer, query string, limit int) (Result, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		if isTimeout(ctx, err) {
			return Result{}, &codedTimeout{err: err}
		}
		if isTooBig(err) {
			return cutByBytes(nil, nil), nil
		}
		return Result{}, err
	}
	defer rows.Close()
	rawCols, err := rows.Columns()
	if err != nil {
		if isTimeout(ctx, err) {
			return Result{}, &codedTimeout{err: err}
		}
		return Result{}, err
	}
	cols := append([]string(nil), rawCols...)
	out := make([][]any, 0)
	truncated := false
	reason := ""
	used := 0
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return Result{}, &codedTimeout{err: err}
		}
		if len(out) == limit {
			truncated = true
			break
		}
		row, err := scanRow(rows, len(cols))
		if err != nil {
			if isTimeout(ctx, err) {
				return Result{}, &codedTimeout{err: err}
			}
			if isTooBig(err) {
				return cutByBytes(cols, out), nil
			}
			return Result{}, err
		}
		n := rowBytes(row)
		if used+n > MaxResultBytes {
			return cutByBytes(cols, out), nil
		}
		used += n
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		if isTimeout(ctx, err) {
			return Result{}, &codedTimeout{err: err}
		}
		if isTooBig(err) {
			return cutByBytes(cols, out), nil
		}
		return Result{}, err
	}
	if err := rows.Close(); err != nil {
		if isTimeout(ctx, err) {
			return Result{}, &codedTimeout{err: err}
		}
		return Result{}, err
	}
	return Result{Columns: cols, Rows: out, RowCount: len(out), Truncated: truncated, TruncateReason: reason}, nil
}

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

func rowBytes(row []any) int {
	n := 0
	for _, cell := range row {
		n += cellBytes(cell)
	}
	return n
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

func isBusy(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) && se != nil {
		code := se.Code() & 0xff
		return code == sqlite3.SQLITE_BUSY || code == sqlite3.SQLITE_LOCKED
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "sqlite_busy") || strings.Contains(msg, "database is locked") || strings.Contains(msg, "sqlite_locked")
}

func scanRow(rows *sql.Rows, n int) ([]any, error) {
	raw := make([]any, n)
	ptrs := make([]any, n)
	for i := range raw {
		ptrs[i] = &raw[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	out := make([]any, n)
	for i, v := range raw {
		out[i] = normalizeValue(v)
	}
	return out, nil
}

func normalizeValue(v any) any {
	b, ok := v.([]byte)
	if !ok {
		return v
	}
	if utf8.Valid(b) {
		return string(b)
	}
	// 非法 UTF-8 不能当成文本，否则 JSON 会换成替换字符。保留字节，编码时走 base64。
	cp := make([]byte, len(b))
	copy(cp, b)
	return cp
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
