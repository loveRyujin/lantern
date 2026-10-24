//go:build linux || darwin

package main

import (
	"fmt"
	"os"
	"syscall"
)

func openConfig(path string) (*os.File, error) {
	before, e := os.Lstat(path)
	if e != nil {
		return nil, e
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("runtime config must be a regular non-symlink file")
	}
	fd, e := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if e != nil {
		return nil, e
	}
	f := os.NewFile(uintptr(fd), path)
	after, e := f.Stat()
	if e != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		f.Close()
		return nil, fmt.Errorf("runtime config changed or is not regular")
	}
	return f, nil
}
