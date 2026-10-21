package sqlx

import (
	"database/sql/driver"
	"reflect"
	"sync"

	"modernc.org/libc"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// resultCellName 包在最终结果的每一列外面。它只数这一列交回模型的字节，
// 一行超过 MaxResultBytes 就让语句失败，后面的列不再计算。
const resultCellName = "lantern_cell"

type cellBudget struct {
	mu sync.Mutex
	n  map[uintptr]int
}

var resultBudget cellBudget

func resultCell(ctx *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	if len(args) != 2 {
		return nil, errRowTooLarge
	}
	col, ok := cellIndex(args[0])
	if !ok {
		return nil, errRowTooLarge
	}
	if err := resultBudget.add(resultCellDB(ctx), col, cellBytes(args[1])); err != nil {
		return nil, err
	}
	return args[1], nil
}

func (b *cellBudget) add(db uintptr, col, n int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.n == nil {
		b.n = map[uintptr]int{}
	}
	if col == 0 {
		b.n[db] = 0
	}
	b.n[db] += n
	if b.n[db] > MaxResultBytes {
		return errRowTooLarge
	}
	return nil
}

func cellIndex(v any) (int, bool) {
	switch n := v.(type) {
	case int64:
		if n < 0 {
			return 0, false
		}
		return int(n), true
	case int:
		return n, n >= 0
	default:
		return 0, false
	}
}

// resultCellDB 取出这条函数调用所在的连接。预算按连接记，避免两条查询互相干扰。
func resultCellDB(ctx *sqlite.FunctionContext) uintptr {
	if ctx == nil {
		return 0
	}
	v := reflect.ValueOf(ctx)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return 0
	}
	v = v.Elem()
	tlsField := v.FieldByName("tls")
	ctxField := v.FieldByName("ctx")
	if !tlsField.IsValid() || !ctxField.IsValid() {
		return 0
	}
	tls, _ := exportedView(tlsField).Interface().(*libc.TLS)
	raw := uintptr(exportedView(ctxField).Uint())
	if tls == nil || raw == 0 {
		return 0
	}
	return sqlite3.Xsqlite3_context_db_handle(tls, raw)
}
