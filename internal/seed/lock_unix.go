//go:build unix

package seed

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockForce 独占 root 下的 `.lantern-seed.lock`。拿不到就返回 errForceBusy，不清理临时目录。
// 进程退出时锁会放开，包括中途崩溃。
func lockForce(root string) (func(), error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("seed: 数据目录: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(root, forceLockName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("seed: 文件锁: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errForceBusy
		}
		return nil, fmt.Errorf("seed: 文件锁: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
