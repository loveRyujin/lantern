package pyrt

import (
	"errors"
	"strings"

	"github.com/loveRyujin/lantern/core/errcode"
)

// ErrUnavailable 表示 Python 运行时不可用。
// 任意 *UnavailableError 都与它匹配。用 errors.Is 判断，不要比较指针。
// 码值是 errcode.RuntimeUnavailable，不要新造一个字符串。
var ErrUnavailable = &UnavailableError{}

// ErrLockfile 表示嵌入的哈希锁定文件本身不合法。
// 这是构建产物坏了，不是用户机器上没有 Python，所以不带 runtime_unavailable。
var ErrLockfile = errors.New("pyrt: 锁定文件无效")

// UnavailableError 是运行时不可用。
// Op、Path、Detail 只给日志看，调用方用 Code 或 errors.Is 分支。
type UnavailableError struct {
	Op     string
	Path   string
	Detail string
}

// Error 实现 error。前缀固定为码值 runtime_unavailable。
func (e *UnavailableError) Error() string {
	if e == nil {
		return string(errcode.RuntimeUnavailable)
	}
	var b strings.Builder
	b.WriteString(string(errcode.RuntimeUnavailable))
	if e.Op != "" {
		b.WriteString(": ")
		b.WriteString(e.Op)
	}
	if e.Path != "" {
		b.WriteString(": ")
		b.WriteString(e.Path)
	}
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	return b.String()
}

// Code 返回 errcode.RuntimeUnavailable。
func (e *UnavailableError) Code() errcode.Code {
	if e == nil {
		return ""
	}
	return errcode.RuntimeUnavailable
}

// Is 按类型匹配。同一类失败互相相等，忽略 Op、Path 和 Detail。
func (e *UnavailableError) Is(target error) bool {
	t, ok := target.(*UnavailableError)
	if !ok || e == nil || t == nil {
		return false
	}
	return true
}

func unavailable(op, path, detail string) error {
	return &UnavailableError{Op: op, Path: path, Detail: detail}
}

// ErrPathEscape 表示 Root 或通往 venv 的路径上有符号链接，或者解析后的位置不在 Root 里。
// 任意 *PathError 都与它匹配。码值是 errcode.PathEscape，不是 runtime_unavailable。
var ErrPathEscape = &PathError{}

// PathError 是路径逃逸。Path 和 Detail 只给日志看。
type PathError struct {
	Path   string
	Detail string
}

// Error 实现 error。前缀固定为码值 path_escape。
func (e *PathError) Error() string {
	if e == nil {
		return string(errcode.PathEscape)
	}
	var b strings.Builder
	b.WriteString(string(errcode.PathEscape))
	if e.Path != "" {
		b.WriteString(": ")
		b.WriteString(e.Path)
	}
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	return b.String()
}

// Code 返回 errcode.PathEscape。
func (e *PathError) Code() errcode.Code {
	if e == nil {
		return ""
	}
	return errcode.PathEscape
}

// Is 按类型匹配。同一类失败互相相等，忽略 Path 和 Detail。
func (e *PathError) Is(target error) bool {
	t, ok := target.(*PathError)
	if !ok || e == nil || t == nil {
		return false
	}
	return true
}

func pathEscape(path, detail string) error {
	return &PathError{Path: path, Detail: detail}
}

// preferCoded 保留已经带稳定错误码的错误。其余运行时失败仍是 runtime_unavailable。
func preferCoded(op, path string, err error) error {
	if err == nil {
		return nil
	}
	if ctxErr(err) {
		return err
	}
	if _, ok := errcode.CodeOf(err); ok {
		return err
	}
	return unavailable(op, path, err.Error())
}
