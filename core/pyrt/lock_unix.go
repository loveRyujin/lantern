//go:build unix

package pyrt

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ensureLockName 是真实 Root 下的 Ensure 文件锁。
// sweepScratch 只看 pyenv 里的 .venv-new-* / .venv-old-*，不会删这个文件。
const ensureLockName = ".pyrt.lock"

// errEnsureBusy 表示同一 Root 已经有一次 Ensure 拿着文件锁。
var errEnsureBusy = errors.New("pyrt: 已有一次 Ensure 在进行")

// lockEnsure 独占 root 下的 .pyrt.lock。拿不到就返回 errEnsureBusy，调用方不得再清理或替换。
// 进程退出时锁会放开，包括中途崩溃。
func lockEnsure(root string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(root, ensureLockName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("pyrt: 文件锁: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errEnsureBusy
		}
		return nil, fmt.Errorf("pyrt: 文件锁: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
