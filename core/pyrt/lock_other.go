//go:build !unix

package pyrt

import "errors"

// ensureLockName 与 Unix 实现同名，避免调用方写成两套路径。
const ensureLockName = ".pyrt.lock"

// errEnsureBusy 在非 Unix 上用不到；锁直接失败，而不是报告“正在进行”。
var errEnsureBusy = errors.New("pyrt: 已有一次 Ensure 在进行")

// lockEnsure 在非 Unix 系统上没有 flock。Ensure 直接失败，避免无锁并发。
func lockEnsure(string) (func(), error) {
	return nil, errors.New("pyrt: 非 Unix 系统没有文件锁，不能执行 Ensure")
}
