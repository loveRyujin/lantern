//go:build unix

package pyrt

import (
	"errors"
	"syscall"
)

// openDirNoFollow 打开目录且不跟随最后一层符号链接。
// 最后一层是符号链接时返回 errSymlink，调用方不得再对这个路径做 RemoveAll。
func openDirNoFollow(path string) error {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return errSymlink
		}
		return err
	}
	return syscall.Close(fd)
}
