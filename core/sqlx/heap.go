package sqlx

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

var (
	heapOnce sync.Once
	heapErr  error
	tempOnce sync.Once
	tempErr  error
	tempKeep uintptr
)

// installHeapLimit 把 SQLite 的硬堆上限设成 HardHeapBytes。
// sqlite3_hard_heap_limit64 是进程级的，工作区、审计和敏感库的写入也共用它。
func installHeapLimit() error {
	heapOnce.Do(func() {
		tls := libc.NewTLS()
		defer tls.Close()
		if sqlite3.Xsqlite3_hard_heap_limit64(tls, HardHeapBytes) < 0 {
			heapErr = fmt.Errorf("sqlx: sqlite3_hard_heap_limit64 失败")
			return
		}
		if got := sqlite3.Xsqlite3_hard_heap_limit64(tls, -1); got != HardHeapBytes {
			heapErr = fmt.Errorf("sqlx: 堆上限 = %d，想要 %d", got, HardHeapBytes)
		}
	})
	return heapErr
}

// installTempDir 把 SQLite 临时文件放到系统临时目录，不放在业务库旁边。
// sqlite3_temp_directory 也是进程级的。目录一旦设上就留着，SQLite 会一直用这块字符串。
func installTempDir() error {
	tempOnce.Do(func() {
		dir := filepath.Join(os.TempDir(), "lantern-sqlite")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			tempErr = fmt.Errorf("sqlx: 临时目录: %w", err)
			return
		}
		tls := libc.NewTLS()
		defer tls.Close()
		cstr, err := libc.CString(dir)
		if err != nil {
			tempErr = err
			return
		}
		if sqlite3.Xsqlite3_temp_directory != 0 {
			libc.Xfree(tls, sqlite3.Xsqlite3_temp_directory)
		}
		sqlite3.Xsqlite3_temp_directory = cstr
		tempKeep = cstr
	})
	return tempErr
}

// tempDir 是测试用来确认临时文件没落在业务库旁边。
func tempDir() string {
	return filepath.Join(os.TempDir(), "lantern-sqlite")
}
