//go:build unix

package pyrt

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

func lockPrepared(root string) (func(), error) {
	path := filepath.Join(root, ensureLockName)
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, fmt.Errorf("prepared lock must be an existing regular file")
	}
	if err = syscall.Flock(fd, syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("prepared runtime is busy: %w", err)
	}
	return func() { _ = syscall.Flock(fd, syscall.LOCK_UN); _ = file.Close() }, nil
}
func readPreparedRegular(path string, limit int64) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, fmt.Errorf("prepared marker must be a bounded regular file")
	}
	body, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("prepared marker exceeds validation budget")
	}
	return body, nil
}
