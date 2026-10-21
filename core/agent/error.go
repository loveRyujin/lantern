package agent

import "fmt"

const (
	// CodeMaxSteps 表示已经走到最大步数，不再调用模型。
	CodeMaxSteps = "max_steps"
	// CodeTimeout 表示运行总时间到了，或工具没有在时限内返回。
	CodeTimeout = "timeout"
	// CodeHashMismatch 表示执行前的参数和校验或确认时的哈希不一致。
	// 批准带回的另一份参数对不上冻结快照，也用这个码。
	CodeHashMismatch = "hash_mismatch"
)

const (
	// StateCompleted 是正常完成。
	StateCompleted = "completed"
	// StateFailed 是失败。连续参数失败也是这个状态，同时错误可重试。
	StateFailed = "failed"
	// StateCancelled 是调用方取消。
	StateCancelled = "cancelled"
	// StateRejected 是确认被拒绝。
	StateRejected = "rejected"
	// StateAwaitingApproval 是确认被暂缓。
	StateAwaitingApproval = "awaiting_approval"
)

// Error 是循环返回的错误。Retryable 为 true 时，这次运行已经结束，调用方可以整段重跑。
type Error struct {
	Code      string
	Retryable bool
	Message   string
	Err       error
}

func (e *Error) Error() string {
	if e == nil {
		return "agent: <nil>"
	}
	msg := e.Message
	if msg == "" {
		msg = e.Code
	}
	if e.Code == "" {
		return "agent: " + msg
	}
	return "agent: " + e.Code + ": " + msg
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

// ErrMaxSteps 是达到最大步数。
var ErrMaxSteps = &Error{Code: CodeMaxSteps, Message: "达到最大步数"}

func agentErr(code, message string, retryable bool) *Error {
	return &Error{Code: code, Retryable: retryable, Message: message}
}

func wrapf(code, message string, retryable bool, err error) *Error {
	return &Error{Code: code, Retryable: retryable, Message: message, Err: err}
}

func plain(format string, args ...any) error {
	return fmt.Errorf("agent: "+format, args...)
}
