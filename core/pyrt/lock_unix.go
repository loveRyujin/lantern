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
// 打开时带 O_NOFOLLOW：最后一层是符号链接就返回 path_escape，不跟随，也不对目标加锁。
// 进程退出时锁会放开，包括中途崩溃。
func lockEnsure(root string) (func(), error) {
	path := filepath.Join(root, ensureLockName)
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o644)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, pathEscape(path, "文件锁是符号链接")
		}
		return nil, fmt.Errorf("pyrt: 文件锁: %w", err)
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("pyrt: 文件锁: 无效的文件描述符")
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
