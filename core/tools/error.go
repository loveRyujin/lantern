package tools

import "errors"

// 参数错误码。改码值算破坏性变更。
// tool_arguments_incomplete 对应评测集 W2-TOOL-001。
// issue #32 正文里的 tool_args_truncated、tool_args_invalid_json、tool_args_schema 不使用。
const (
	// CodeArgumentsIncomplete 表示输出被截断，参数不能执行。可重试。
	CodeArgumentsIncomplete = "tool_arguments_incomplete"
	// CodeArgumentsInvalid 表示 JSON 不合法或不符合 Schema。可重试。
	CodeArgumentsInvalid = "tool_arguments_invalid"
	// CodeCallSkipped 表示同一步里有调用不合法，这个本来合法的调用没有执行。
	CodeCallSkipped = "tool_call_skipped"
	// CodeToolNotFound 表示注册表里没有这个名字。可重试。
	// 它和参数校验失败一样计入连续失败，码值仍是 tool_not_found。
	CodeToolNotFound = "tool_not_found"
)

// Error 是工具参数或注册表返回的错误。Message 给模型看，不带路径和堆栈。
type Error struct {
	Code      string
	Retryable bool
	Message   string
	Err       error
}

func (e *Error) Error() string {
	if e == nil {
		return "tools: <nil>"
	}
	msg := e.Message
	if msg == "" {
		msg = e.Code
	}
	if e.Code == "" {
		return "tools: " + msg
	}
	return "tools: " + e.Code + ": " + msg
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Is 按错误码匹配。
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok || e == nil || t == nil || t.Code == "" {
		return false
	}
	return e.Code == t.Code
}

// CodeOf 取出工具错误码。
func CodeOf(err error) (string, bool) {
	var e *Error
	if !errors.As(err, &e) || e == nil || e.Code == "" {
		return "", false
	}
	return e.Code, true
}

// ArgumentFailure 报告这是不是计入连续失败的调用校验错误。
// 截断、非法 JSON、Schema 不符，以及未知工具都算。
// tool_call_skipped 只是同一步被连带取消，不单独计数。
func ArgumentFailure(err error) bool {
	code, ok := CodeOf(err)
	if !ok {
		return false
	}
	return code == CodeArgumentsIncomplete || code == CodeArgumentsInvalid || code == CodeToolNotFound
}

func incomplete(message string) *Error {
	if message == "" {
		message = "参数因输出截断而不完整"
	}
	return &Error{Code: CodeArgumentsIncomplete, Retryable: true, Message: message}
}

func invalidArgs(message string) *Error {
	if message == "" {
		message = "参数不合法"
	}
	return &Error{Code: CodeArgumentsInvalid, Retryable: true, Message: message}
}

// Skip 是被同一步里其他不合法调用连带取消的结果。
func Skip() *Error {
	return &Error{
		Code:      CodeCallSkipped,
		Retryable: true,
		Message:   "同一步里有调用不合法，本调用未执行",
	}
}
