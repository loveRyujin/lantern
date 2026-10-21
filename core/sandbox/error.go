package sandbox

import (
	"errors"

	"github.com/loveRyujin/lantern/core/errcode"
)

// ExecError 是一次隔离执行的失败。
// Kind 是稳定错误码。Detail 是给日志看的短说明，不作为分支条件。
type ExecError struct {
	Kind   errcode.Code
	Detail string
}

// Error 实现 error。格式是「码值: 说明」；没有说明时只有码值。
func (e *ExecError) Error() string {
	if e == nil {
		return ""
	}
	if e.Detail == "" {
		return string(e.Kind)
	}
	return string(e.Kind) + ": " + e.Detail
}

// Code 返回稳定错误码。
func (e *ExecError) Code() errcode.Code {
	if e == nil {
		return ""
	}
	return e.Kind
}

// Is 按错误码比较。同一码值的 ExecError 互相匹配，忽略 Detail。
func (e *ExecError) Is(target error) bool {
	t, ok := target.(*ExecError)
	if !ok || e == nil || t == nil || t.Kind == "" {
		return false
	}
	return e.Kind == t.Kind
}

// 哨兵按错误码匹配任意同类失败。用 errors.Is 判断，不要比较指针。
var (
	ErrTimeout            = &ExecError{Kind: errcode.Timeout}
	ErrOOM                = &ExecError{Kind: errcode.OOM}
	ErrKilled             = &ExecError{Kind: errcode.Killed}
	ErrFailed             = &ExecError{Kind: errcode.Exec}
	ErrRuntimeUnavailable = &ExecError{Kind: errcode.RuntimeUnavailable}
	ErrSandboxUnavailable = &ExecError{Kind: errcode.SandboxUnavailable}
)

// Timeout 构造墙钟超时或上下文取消导致的失败。
func Timeout(detail string) error {
	return &ExecError{Kind: errcode.Timeout, Detail: detail}
}

// OOM 构造内存超限。
func OOM(detail string) error {
	return &ExecError{Kind: errcode.OOM, Detail: detail}
}

// Killed 构造被信号杀死。
func Killed(detail string) error {
	return &ExecError{Kind: errcode.Killed, Detail: detail}
}

// Failed 构造未归类的执行失败，错误码是 error。
func Failed(detail string) error {
	return &ExecError{Kind: errcode.Exec, Detail: detail}
}

// RuntimeUnavailable 构造运行时不可用。调用方不得因此改走无沙箱执行。
func RuntimeUnavailable(detail string) error {
	return &ExecError{Kind: errcode.RuntimeUnavailable, Detail: detail}
}

// SandboxUnavailable 构造隔离机制不可用。调用方不得因此改用更弱的隔离。
func SandboxUnavailable(detail string) error {
	return &ExecError{Kind: errcode.SandboxUnavailable, Detail: detail}
}

func align(res Result, err error) (Result, error) {
	if err == nil {
		res.ErrKind = ""
		return cloneResult(res), nil
	}
	var exec *ExecError
	if errors.As(err, &exec) && exec != nil {
		res.ErrKind = exec.Code()
	}
	return cloneResult(res), err
}
