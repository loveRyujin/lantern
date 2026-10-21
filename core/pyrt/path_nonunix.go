//go:build !unix

package pyrt

import "os"

// openDirNoFollow 在非 Unix 上用 Lstat 确认最后一层不是符号链接。
func openDirNoFollow(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errSymlink
	}
	if !info.IsDir() {
		return os.ErrInvalid
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return f.Close()
}
