//go:build !unix

package seed

import "errors"

// lockForce 在非 Unix 系统上没有文件锁。-force 直接失败，避免静默并发。
func lockForce(string) (func(), error) {
	return nil, errors.New("seed: -force file lock is not supported")
}
