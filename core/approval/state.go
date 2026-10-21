package approval

import (
	"errors"

	"github.com/loveRyujin/lantern/core/errcode"
)

// State 是确认单状态。只有状态机能改它。
type State string

const (
	// StatePending 是已落库、还没有决定。
	StatePending State = "pending"
	// StateApproved 是已批准、还没消费。
	StateApproved State = "approved"
	// StateRejected 是已拒绝。终态。
	StateRejected State = "rejected"
	// StateExpired 是过了期限。终态。
	StateExpired State = "expired"
	// StateConsumed 是已经消费。终态，之后不能再转。
	StateConsumed State = "consumed"
)

// ConsumeContext 是调用方从当前步骤带来的核对信息。
// 状态机拿它和确认单比较，不会从确认单自己反推运行或步骤。
type ConsumeContext struct {
	RunID    string
	StepID   string
	ArgsHash string
}

// 状态机返回的错误。调用方用 errors.Is 判断。
var (
	// ErrIllegalTransition 表示这次状态转移不在允许表里。
	ErrIllegalTransition = errors.New("approval: illegal transition")
	// ErrTerminal 表示确认单已经是终态，不能再操作。
	ErrTerminal = errors.New("approval: terminal state")
	// ErrVersionConflict 表示版本号已经被别人改过，这次条件更新没有生效。
	ErrVersionConflict = errors.New("approval: version conflict")
	// ErrMismatch 表示运行、步骤或参数哈希和确认单不一致。状态不变。
	ErrMismatch error = mismatchError{}
	// ErrCorrupt 表示 state 和 decision、consumed_at 对不上，拒绝继续。
	ErrCorrupt = errors.New("approval: corrupt approval")
	// ErrExpired 表示确认单已经因为期限到了而失效。
	ErrExpired = errors.New("approval: expired")
	// ErrNotFound 表示没有这张确认单。
	ErrNotFound = errors.New("approval: not found")
	// ErrNotDue 表示还没到 expires_at，不能写成过期。
	ErrNotDue = errors.New("approval: not due")
	// ErrScope 表示工作区、动作或导出对象和这张确认单不一致。
	ErrScope = errors.New("approval: export scope")
)

// mismatchError 的文本就是稳定错误码 approval_mismatch。
type mismatchError struct{}

func (mismatchError) Error() string { return "approval_mismatch" }

func (mismatchError) Code() errcode.Code { return errcode.ApprovalMismatch }

func terminal(state State) bool {
	return state == StateRejected || state == StateExpired || state == StateConsumed
}

func legal(from, to State) bool {
	switch {
	case from == StatePending && (to == StateApproved || to == StateRejected || to == StateExpired):
		return true
	case from == StateApproved && (to == StateConsumed || to == StateExpired):
		return true
	default:
		return false
	}
}

// transitionError 让非法转移同时能被 ErrIllegalTransition 认出来。
// 终态上的操作还能被 ErrTerminal 认出来。
type transitionError struct {
	from, to State
}

func (e *transitionError) Error() string {
	return "approval: illegal transition " + string(e.from) + " -> " + string(e.to)
}

func (e *transitionError) Is(target error) bool {
	if target == ErrIllegalTransition {
		return true
	}
	if target == ErrTerminal {
		return terminal(e.from)
	}
	return false
}
