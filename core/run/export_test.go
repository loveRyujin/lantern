package run

import (
	"context"
	"time"
)

// SetBusyBudget 换掉 SQLITE_BUSY 的重试预算，返回恢复函数。只给测试用。
func SetBusyBudget(d time.Duration) (restore func()) {
	old := busyBudget
	busyBudget = d
	return func() { busyBudget = old }
}

// ForceState 暴露 forceState，终态保护测试用。
func (s *Service) ForceState(ctx context.Context, runID, to string) error {
	return s.forceState(ctx, runID, to)
}
