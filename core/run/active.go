package run

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
)

// 本进程里正在推进的运行。键是 runs.db 的文件路径加运行标识：同一个工作区每次 Open
// 都是新的 *sql.DB，所以不能按连接池区分；不同工作区可以有同名运行，所以要带路径。
var active = struct {
	mu sync.Mutex
	n  map[activeKey]int
}{n: map[activeKey]int{}}

type activeKey struct {
	db  string
	run string
}

// Hold 把 runID 登记为本进程活跃，直到调用返回的 release。Recover 跳过已登记的运行，
// 不把它们当成残留标为 failed。同一运行可以重复登记，全部 release 之后才算结束。
// release 可以多次调用，只有第一次生效。
//
// Service.Resume 自己会登记；Agent 循环由调用方（session.LoopRunner.Ask）在写入运行行之前登记。
// 必须在第一次写这趟运行之前登记，否则 Recover 可能在登记前看到半截状态。
// 进程重启后登记表为空，这时 Recover 就是完整的启动扫描。
func Hold(ctx context.Context, db *sql.DB, runID string) (release func(), err error) {
	if ctx == nil {
		return nil, errors.New("run: ctx 不能为 nil")
	}
	if db == nil {
		return nil, ErrNoDB
	}
	if runID == "" {
		return nil, errors.New("run: 运行标识为空")
	}
	path, err := dbIdentity(ctx, db)
	if err != nil {
		return nil, err
	}
	k := activeKey{db: path, run: runID}
	active.mu.Lock()
	active.n[k]++
	active.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			active.mu.Lock()
			if active.n[k] <= 1 {
				delete(active.n, k)
			} else {
				active.n[k]--
			}
			active.mu.Unlock()
		})
	}, nil
}

func held(path, runID string) bool {
	active.mu.Lock()
	defer active.mu.Unlock()
	return active.n[activeKey{db: path, run: runID}] > 0
}

// dbIdentity 是 main 库的文件路径。内存库没有路径，退回到连接池地址。
func dbIdentity(ctx context.Context, db *sql.DB) (string, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA database_list`)
	if err != nil {
		return "", fmt.Errorf("run: 读取库路径: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var seq int
		var name, file string
		if err := rows.Scan(&seq, &name, &file); err != nil {
			return "", fmt.Errorf("run: 读取库路径: %w", err)
		}
		if name == "main" {
			if file == "" {
				return fmt.Sprintf("mem:%p", db), nil
			}
			return file, nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("run: 读取库路径: %w", err)
	}
	return "", errors.New("run: 没有 main 库")
}
