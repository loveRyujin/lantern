package sqlx

import (
	"fmt"
	"reflect"
	"strings"
	"unsafe"

	"modernc.org/libc"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// RejectAttach 在一条 modernc 连接上拒绝 ATTACH。
// 工作区里可写的库和这里的只读连接都要装上。modernc 的 Driver 文档写明它不暴露
// authorizer；sqlite.Limit 又要已经包进 database/sql 的 *sql.Conn，连接钩子里还拿不到。
// 钩子拿到的是驱动自己的连接，这里直接调用 lib 里的 sqlite3_limit 和 sqlite3_set_authorizer。
// 读不到连接内部字段就拒绝打开，避免静默退回到没有防护的连接。
//
// 授权回调只拒绝 SQLITE_ATTACH。迁移和业务写入不受影响，query_only 也不在这里开关。
func RejectAttach(conn sqlite.ExecQuerierContext) error {
	db, tls, err := sqliteConn(conn)
	if err != nil {
		return err
	}
	if got := sqlite3.Xsqlite3_limit(tls, db, sqlite3.SQLITE_LIMIT_ATTACHED, 0); got < 0 {
		return fmt.Errorf("sqlx: sqlite3_limit(SQLITE_LIMIT_ATTACHED): %d", got)
	}
	if got := sqlite3.Xsqlite3_limit(tls, db, sqlite3.SQLITE_LIMIT_ATTACHED, -1); got != 0 {
		return fmt.Errorf("sqlx: SQLITE_LIMIT_ATTACHED = %d，想要 0", got)
	}
	rc := sqlite3.Xsqlite3_set_authorizer(tls, db, cFuncPointer(denyAttach), 0)
	if rc != sqlite3.SQLITE_OK {
		return fmt.Errorf("sqlx: sqlite3_set_authorizer: %d", rc)
	}
	return nil
}

// denyAttach 是 sqlite3_set_authorizer 的回调。必须是包级函数：modernc 用函数值的
// 代码指针注册回调，闭包的表示和这个约定不一致。除 SQLITE_ATTACH 以外全部放行。
func denyAttach(_ *libc.TLS, _ uintptr, action int32, _, _, _, _ uintptr) int32 {
	if action == sqlite3.SQLITE_ATTACH {
		return sqlite3.SQLITE_DENY
	}
	return sqlite3.SQLITE_OK
}

// lockReadOnly 换成只读连接用的授权回调。要在 PRAGMA query_only=ON 之后调用，
// 否则这次初始化自己会被拦住。工作区的可写连接继续用 [RejectAttach]，不走这里。
func lockReadOnly(conn sqlite.ExecQuerierContext) error {
	db, tls, err := sqliteConn(conn)
	if err != nil {
		return err
	}
	rc := sqlite3.Xsqlite3_set_authorizer(tls, db, cFuncPointer(denyReadOnly), 0)
	if rc != sqlite3.SQLITE_OK {
		return fmt.Errorf("sqlx: sqlite3_set_authorizer: %d", rc)
	}
	return nil
}

// denyReadOnly 拒绝 ATTACH，也拒绝把 query_only 关掉或打开 writable_schema。
// 读取 PRAGMA 的语句没有右值，仍然放行，这样调用方可以确认 query_only 还开着。
// pragma_* 表值函数按授权回调里的函数名或表名拒绝，不靠 SQL 原文。
func denyReadOnly(_ *libc.TLS, _ uintptr, action int32, arg1, arg2, _, _ uintptr) int32 {
	if action == sqlite3.SQLITE_ATTACH {
		return sqlite3.SQLITE_DENY
	}
	if action == sqlite3.SQLITE_FUNCTION && isPragmaIdent(strings.ToLower(cString(arg2))) {
		return sqlite3.SQLITE_DENY
	}
	if action == sqlite3.SQLITE_READ && strings.HasPrefix(strings.ToLower(cString(arg1)), "pragma_") {
		return sqlite3.SQLITE_DENY
	}
	if action == sqlite3.SQLITE_PRAGMA && arg2 != 0 {
		switch strings.ToLower(cString(arg1)) {
		case "query_only", "writable_schema":
			return sqlite3.SQLITE_DENY
		}
	}
	return sqlite3.SQLITE_OK
}

// limitResultLength 让单个单元格到不了结果字节上限以上。超限时驱动返回 SQLITE_TOOBIG，
// 查询层把它记成按字节截断，而不是把整格读进内存再交出去。
func limitResultLength(conn sqlite.ExecQuerierContext) error {
	db, tls, err := sqliteConn(conn)
	if err != nil {
		return err
	}
	if got := sqlite3.Xsqlite3_limit(tls, db, sqlite3.SQLITE_LIMIT_LENGTH, MaxResultBytes); got < 0 {
		return fmt.Errorf("sqlx: sqlite3_limit(SQLITE_LIMIT_LENGTH): %d", got)
	}
	if got := sqlite3.Xsqlite3_limit(tls, db, sqlite3.SQLITE_LIMIT_LENGTH, -1); got != MaxResultBytes {
		return fmt.Errorf("sqlx: SQLITE_LIMIT_LENGTH = %d，想要 %d", got, MaxResultBytes)
	}
	return nil
}

func cString(p uintptr) string {
	if p == 0 {
		return ""
	}
	return libc.GoString(p)
}

// cFuncPointer 与 modernc.org/sqlite 内部把 Go 函数交给 ccgo 的方式相同。
func cFuncPointer[T any](f T) uintptr {
	return *(*uintptr)(unsafe.Pointer(&struct{ f T }{f}))
}

func sqliteConn(conn sqlite.ExecQuerierContext) (uintptr, *libc.TLS, error) {
	v := reflect.ValueOf(conn)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return 0, nil, fmt.Errorf("sqlx: sqlite 连接类型 %T 没有句柄", conn)
	}
	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return 0, nil, fmt.Errorf("sqlx: sqlite 连接类型 %T 没有句柄", conn)
	}
	dbField := v.FieldByName("db")
	tlsField := v.FieldByName("tls")
	if !dbField.IsValid() || !tlsField.IsValid() {
		return 0, nil, fmt.Errorf("sqlx: sqlite 连接类型 %T 没有 db/tls 字段，拒绝打开没有 ATTACH 防护的连接", conn)
	}
	dbField = exportedView(dbField)
	tlsField = exportedView(tlsField)
	db := uintptr(dbField.Uint())
	tls, ok := tlsField.Interface().(*libc.TLS)
	if !ok || tls == nil || db == 0 {
		return 0, nil, fmt.Errorf("sqlx: sqlite 连接类型 %T 的句柄是空的", conn)
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
