package sqlx

import (
	"context"
	"fmt"
	"strconv"
	"sync/atomic"
	"unsafe"

	"modernc.org/libc"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// stepQuery 在第一步之前看结果列数。超过 MaxResultColumns 就拒绝，语句还没执行。
// 每一列先看 sqlite3_column_bytes，再决定要不要拷进 Go。一行超过 MaxResultBytes 就整次失败。
func stepQuery(ctx context.Context, driverConn any, query string, limit int) (Result, error) {
	eq, ok := driverConn.(sqlite.ExecQuerierContext)
	if !ok {
		return Result{}, errNotSQLite(driverConn)
	}
	db, tls, err := sqliteConn(eq)
	if err != nil {
		return Result{}, err
	}
	stop := watchCancel(ctx, tls, db)
	defer stop()
	stmt, err := prepare(tls, db, query)
	if err != nil {
		return Result{}, classifyStep(ctx, err)
	}
	defer sqlite3.Xsqlite3_finalize(tls, stmt)

	n := int(sqlite3.Xsqlite3_column_count(tls, stmt))
	if n > MaxResultColumns {
		return Result{}, errTooManyColumns
	}
	cols := make([]string, n)
	for i := range cols {
		cols[i] = cString(sqlite3.Xsqlite3_column_name(tls, stmt, int32(i)))
	}
	out := make([][]any, 0)
	used := 0
	truncated := false
	for {
		if err := ctx.Err(); err != nil {
			return Result{}, &codedTimeout{err: err}
		}
		rc := sqlite3.Xsqlite3_step(tls, stmt)
		switch rc {
		case sqlite3.SQLITE_ROW:
			// 这一行还在 SQLite 的寄存器里。测试在这里量驱动的内存。
			if onSQLiteRow != nil {
				onSQLiteRow()
			}
			// 正好读满 limit 行时再看到一行，才标截断。正好 limit 行不能标 truncated。
			if len(out) == limit {
				truncated = true
				return Result{Columns: cols, Rows: out, RowCount: len(out), Truncated: truncated}, nil
			}
			row, nbyte, err := readRow(tls, stmt, n)
			if err != nil {
				return Result{}, classifyStep(ctx, err)
			}
			if used+nbyte > MaxResultBytes {
				if len(out) == 0 {
					return Result{}, errRowTooLarge
				}
				return cutByBytes(cols, out), nil
			}
			used += nbyte
			out = append(out, row)
		case sqlite3.SQLITE_DONE:
			return Result{Columns: cols, Rows: out, RowCount: len(out), Truncated: truncated}, nil
		default:
			return Result{}, classifyStep(ctx, stepErr(tls, db, rc))
		}
	}
}

// onSQLiteRow 只给测试用。生产路径保持空。
var onSQLiteRow func()

func errNotSQLite(conn any) error {
	return fmt.Errorf("sqlx: 连接 %T 不是 sqlite", conn)
}

func prepare(tls *libc.TLS, db uintptr, query string) (uintptr, error) {
	sqlPtr, err := libc.CString(query)
	if err != nil {
		return 0, err
	}
	defer libc.Xfree(tls, sqlPtr)
	ppstmt := libc.Xmalloc(tls, libc.Tsize_t(unsafe.Sizeof(uintptr(0))))
	if ppstmt == 0 {
		return 0, fmt.Errorf("sqlx: 准备语句时没有内存")
	}
	defer libc.Xfree(tls, ppstmt)
	slot := libc.GoBytes(ppstmt, int(unsafe.Sizeof(uintptr(0))))
	for i := range slot {
		slot[i] = 0
	}
	rc := sqlite3.Xsqlite3_prepare_v2(tls, db, sqlPtr, -1, ppstmt, 0)
	stmt := loadUintptr(ppstmt)
	if rc != sqlite3.SQLITE_OK {
		if stmt != 0 {
			sqlite3.Xsqlite3_finalize(tls, stmt)
		}
		return 0, stepErr(tls, db, rc)
	}
	if stmt == 0 {
		return 0, fmt.Errorf("sqlx: 没有语句")
	}
	return stmt, nil
}

// readRow 逐列看 sqlite3_column_bytes。这一列放不进 1 MiB 时，不拷贝它，整行失败。
func readRow(tls *libc.TLS, stmt uintptr, n int) ([]any, int, error) {
	row := make([]any, n)
	sum := 0
	for i := range n {
		typ := sqlite3.Xsqlite3_column_type(tls, stmt, int32(i))
		nb, err := columnSize(tls, stmt, int32(i), typ)
		if err != nil {
			return nil, 0, err
		}
		if sum+nb > MaxResultBytes {
			return nil, 0, errRowTooLarge
		}
		val, err := copyColumn(tls, stmt, int32(i), typ, nb)
		if err != nil {
			return nil, 0, err
		}
		sum += nb
		row[i] = val
	}
	return row, sum, nil
}

func columnSize(tls *libc.TLS, stmt uintptr, i, typ int32) (int, error) {
	switch typ {
	case sqlite3.SQLITE_NULL:
		return 0, nil
	case sqlite3.SQLITE_INTEGER:
		v := sqlite3.Xsqlite3_column_int64(tls, stmt, i)
		return len(strconv.FormatInt(int64(v), 10)), nil
	case sqlite3.SQLITE_FLOAT:
		v := sqlite3.Xsqlite3_column_double(tls, stmt, i)
		return len(strconv.FormatFloat(v, 'f', -1, 64)), nil
	case sqlite3.SQLITE_TEXT, sqlite3.SQLITE_BLOB:
		n := int(sqlite3.Xsqlite3_column_bytes(tls, stmt, i))
		if n < 0 {
			return 0, fmt.Errorf("sqlx: 列长度 %d", n)
		}
		return n, nil
	default:
		return 0, fmt.Errorf("sqlx: 未知列类型 %d", typ)
	}
}

func copyColumn(tls *libc.TLS, stmt uintptr, i, typ int32, n int) (any, error) {
	switch typ {
	case sqlite3.SQLITE_NULL:
		return nil, nil
	case sqlite3.SQLITE_INTEGER:
		return int64(sqlite3.Xsqlite3_column_int64(tls, stmt, i)), nil
	case sqlite3.SQLITE_FLOAT:
		return sqlite3.Xsqlite3_column_double(tls, stmt, i), nil
	case sqlite3.SQLITE_TEXT:
		return copyText(tls, stmt, i, n), nil
	case sqlite3.SQLITE_BLOB:
		return copyBlob(tls, stmt, i, n), nil
	default:
		return nil, fmt.Errorf("sqlx: 未知列类型 %d", typ)
	}
}

func copyText(tls *libc.TLS, stmt uintptr, i int32, n int) string {
	if n == 0 {
		return ""
	}
	p := sqlite3.Xsqlite3_column_text(tls, stmt, i)
	b := copyCBytes(p, n)
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(unsafe.SliceData(b), len(b))
}

func copyBlob(tls *libc.TLS, stmt uintptr, i int32, n int) []byte {
	if n == 0 {
		return []byte{}
	}
	b := copyCBytes(sqlite3.Xsqlite3_column_blob(tls, stmt, i), n)
	if b == nil {
		return []byte{}
	}
	return b
}

// copyCBytes 把 SQLite 里的字节拷进 Go。GoBytes 只是借出指针，不拷贝。
func copyCBytes(p uintptr, n int) []byte {
	if p == 0 || n == 0 {
		return nil
	}
	src := libc.GoBytes(p, n)
	dst := make([]byte, len(src))
	copy(dst, src)
	return dst
}

func loadUintptr(p uintptr) uintptr {
	b := libc.GoBytes(p, int(unsafe.Sizeof(uintptr(0))))
	var v uintptr
	for i, c := range b {
		v |= uintptr(c) << (8 * i)
	}
	return v
}

func stepErr(tls *libc.TLS, db uintptr, rc int32) error {
	if rc == sqlite3.SQLITE_TOOBIG || rc == sqlite3.SQLITE_NOMEM || rc == sqlite3.SQLITE_IOERR_NOMEM {
		return errRowTooLarge
	}
	msg := cString(sqlite3.Xsqlite3_errmsg(tls, db))
	switch rc {
	case sqlite3.SQLITE_INTERRUPT:
		if msg == "" {
			msg = "interrupted"
		}
		return fmt.Errorf("sqlite interrupt: %s", msg)
	case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
		return fmt.Errorf("database is locked")
	default:
		if msg == "" {
			return fmt.Errorf("sqlite %d", rc)
		}
		return fmt.Errorf("%s", msg)
	}
}

func classifyStep(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if isTimeout(ctx, err) {
		return &codedTimeout{err: err}
	}
	if rowTooLarge(err) {
		return errRowTooLarge
	}
	return err
}

func watchCancel(ctx context.Context, tls *libc.TLS, db uintptr) func() {
	if ctx == nil || ctx.Done() == nil {
		return func() {}
	}
	var done atomic.Int32
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			if done.CompareAndSwap(0, 1) {
				sqlite3.Xsqlite3_interrupt(tls, db)
			}
		case <-stop:
		}
	}()
	return func() {
		done.Store(1)
		close(stop)
	}
}
