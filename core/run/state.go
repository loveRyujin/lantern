package run

import (
	"errors"

	"github.com/loveRyujin/lantern/core/approval"
)

const (
	// StateCreated 是已经落库、循环还没开始。
	StateCreated = "created"
	// StateRunning 是循环正在跑。
	StateRunning = "running"
	// StateAwaitingApproval 是确认被暂缓，或已批准但还没消费。
	StateAwaitingApproval = "awaiting_approval"
	// StateRejected 是确认被拒绝。
	StateRejected = "rejected"
	// StateExpired 是确认单过期。
	StateExpired = "expired"
	// StateCompleted 是正常完成。
	StateCompleted = "completed"
	// StateFailed 是失败。中断的执行和不合法残留也进这里。
	StateFailed = "failed"
	// StateCancelled 是调用方取消。
	StateCancelled = "cancelled"
)

const (
	// StepPending 是步骤已记录但还没开始执行。
	StepPending = "pending"
	// StepExecuting 是工具已经开始、结果还没落库。
	StepExecuting = "executing"
	// StepDone 是工具已经成功返回。
	StepDone = "done"
	// StepError 是工具返回了错误，运行没有停在执行中。
	StepError = "error"
)

const (
	// AuditApprovalRestored 表示挂起的确认在重启后仍然可以决定。与 AP-010 的事件名一致。
	AuditApprovalRestored = "approval_restored"
	// AuditExecutingInterrupted 表示卡在 executing 的运行被标为失败，没有重跑。与 AP-012 一致。
	AuditExecutingInterrupted = "executing_interrupted"
	// AuditIllegalResidual 表示没有明确断点的未终结运行被标为失败。
	AuditIllegalResidual = "illegal_residual"
	// AuditBreakpointHeld 表示 running 且最后一步为 done。扫描不续跑模型。
	AuditBreakpointHeld = "breakpoint_held"
	// AuditHashMismatch 表示执行前的参数和冻结哈希不一致。与 Agent 循环的审计动作同名。
	AuditHashMismatch = "hash_mismatch"
	// AuditToolFailed 表示恢复后的那一次执行失败。不会再自动重跑。
	AuditToolFailed = "tool_failed"
)

var (
	// ErrNoDB 表示没有 runs 库。
	ErrNoDB = errors.New("run: 没有数据库")
	// ErrNotFound 表示没有这趟运行。
	ErrNotFound = errors.New("run: 运行不存在")
	// ErrNotAwaiting 表示 Resume 只能处理仍挂起的运行。
	ErrNotAwaiting = errors.New("run: 运行不在 awaiting_approval")
	// ErrConflict 表示版本已经被别的写入改过。
	ErrConflict = errors.New("run: 版本冲突")
	// ErrNoApprover 表示挂起的确认单还要决定，但没有 Approver。
	ErrNoApprover = errors.New("run: 没有 Approver")
	// ErrNoExecutor 表示已经可以执行，但没有执行器。
	ErrNoExecutor = errors.New("run: 没有执行器")
	// ErrInterrupted 表示步骤停在 executing，运行已标为失败，工具不会重跑。
	ErrInterrupted = errors.New("run: 执行中断，已标为失败")
	// ErrStepExists 表示这个步骤号已经落过库。
	ErrStepExists = errors.New("run: 步骤已存在")
)

// hashMismatch 是执行前哈希不一致。Unwrap 到审批包的哨兵，调用方不用再写一套判断。
type hashMismatch struct{}

func (hashMismatch) Error() string { return "run: hash_mismatch: 参数快照哈希不一致" }

func (hashMismatch) Unwrap() error { return approval.ErrSnapshotMismatch }

type stepExistsError struct {
	Status string
}

func (e *stepExistsError) Error() string { return ErrStepExists.Error() }

func (e *stepExistsError) Is(target error) bool { return target == ErrStepExists }

func knownState(state string) bool {
	switch state {
	case StateCreated, StateRunning, StateAwaitingApproval, StateRejected, StateExpired, StateCompleted, StateFailed, StateCancelled:
		return true
	default:
		return false
	}
}

func terminal(state string) bool {
	switch state {
	case StateCompleted, StateFailed, StateCancelled, StateRejected, StateExpired:
		return true
	default:
		return false
	}
}
