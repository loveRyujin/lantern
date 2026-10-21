package pyrt

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"time"
)

type cmdResult struct {
	Stdout []byte
	Stderr []byte
}

type limitedBuf struct {
	buf   bytes.Buffer
	max   int
	trunc bool
}

func (l *limitedBuf) Write(p []byte) (int, error) {
	if l.max <= 0 {
		l.max = 1 << 20
	}
	room := l.max - l.buf.Len()
	if room <= 0 {
		l.trunc = true
		return len(p), nil
	}
	if len(p) > room {
		l.trunc = true
		p = p[:room]
	}
	_, _ = l.buf.Write(p)
	return len(p), nil
}

func runCmd(ctx context.Context, name string, args ...string) (cmdResult, error) {
	if err := ctx.Err(); err != nil {
		return cmdResult{}, err
	}
	cmd := exec.CommandContext(ctx, name, args...)
	// 上下文取消后，子进程的孙进程可能还握着管道。WaitDelay 防止 Wait 一直等到 EOF。
	cmd.WaitDelay = 2 * time.Second
	configureCancel(cmd)
	var stdout, stderr limitedBuf
	stdout.max = 1 << 20
	stderr.max = 256 << 10
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := cmdResult{Stdout: stdout.buf.Bytes(), Stderr: stderr.buf.Bytes()}
	if err != nil && ctx.Err() != nil {
		return res, ctx.Err()
	}
	return res, err
}

func firstOutput(res cmdResult) []byte {
	if strings.TrimSpace(string(res.Stderr)) != "" {
		return res.Stderr
	}
	return res.Stdout
}

func trimDetail(b []byte) string {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "命令失败"
	}
	r := []rune(s)
	const max = 800
	if len(r) > max {
		return string(r[:max]) + "…(已截断)"
	}
	return s
}
