//go:build !unix

package pyrt

import "os/exec"

// configureCancel 在非 Unix 上沿用 CommandContext 默认的 Process.Kill。
func configureCancel(*exec.Cmd) {}
