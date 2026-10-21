package sqlx

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/loveRyujin/lantern/core/errcode"
)

// Queryer 是只读查询用的连接池。*sql.DB 满足它。
type Queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Result 是截断之后的查询结果。RowCount 是实际返回的行数，不是库里的总行数。
type Result struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	RowCount  int      `json:"row_count"`
	Truncated bool     `json:"truncated"`
}

// Query 执行一条已经通过扫描的语句，最多取 limit 行。
// 不改写 SQL。多读一行只为了判断要不要标 truncated。limit 必须在 1 和 MaxLimit 之间。
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
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		if isTimeout(ctx, err) {
			return Result{}, &codedTimeout{err: err}
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
	for rows.Next() {
		if len(out) == limit {
			truncated = true
			break
		}
		row, err := scanRow(rows, len(cols))
		if err != nil {
			if isTimeout(ctx, err) {
				return Result{}, &codedTimeout{err: err}
			}
			return Result{}, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		if isTimeout(ctx, err) {
			return Result{}, &codedTimeout{err: err}
		}
		return Result{}, err
	}
	if err := rows.Close(); err != nil {
		if isTimeout(ctx, err) {
			return Result{}, &codedTimeout{err: err}
		}
		return Result{}, err
	}
	return Result{Columns: cols, Rows: out, RowCount: len(out), Truncated: truncated}, nil
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
	return strings.Clone(string(b))
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
