//go:build !linux && !darwin

package sqlx

import (
	"context"
	"os"

	"github.com/loveRyujin/lantern/core/tools"
)

func fileIdentity(os.FileInfo) (uint64, uint64) { return 0, 0 }

// Linux 与 Darwin 有各自的隔离进程。其余平台在实现系统资源限制前拒绝执行，不退回主进程。
func runSQLProcess(context.Context, processRequest) (Result, processStats, error) {
	return Result{}, processStats{}, &tools.Error{Code: CodeWorkerUnavailable, Message: "当前平台尚未实现 SQL 隔离进程"}
}
