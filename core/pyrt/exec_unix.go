//go:build unix

package pyrt

import (
	"errors"
	"os/exec"
	"syscall"
)

// configureCancel 让取消时杀掉整个进程组。
// pip 和 venv 都会再拉起子进程；只杀直接子进程的话，孙进程可能继续占着管道。
func configureCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err != nil && !errors.Is(err, syscall.ESRCH) {
			_ = cmd.Process.Kill()
			return err
		}
		return nil
	}
}
