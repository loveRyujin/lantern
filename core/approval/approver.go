package approval

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// Action 是确认单上的决定。
type Action string

const (
	// ActionApprove 表示按冻结的参数快照执行。
	// Decision.Params 非 nil 时表示有人试图替换参数，执行前必须重新哈希。
	ActionApprove Action = "approve"
	// ActionReject 表示拒绝。运行不应执行该工具。
	ActionReject Action = "reject"
	// ActionDefer 表示暂缓。Escape 关闭弹窗走这条路径，运行保持 awaiting_approval。
	ActionDefer Action = "defer"
)

// Ticket 是已经落库的确认单。字段对应确认弹窗要展示的内容。
type Ticket struct {
	// ID 是确认单标识。
	ID string
	// RunID 是所属运行。
	RunID string
	// WorkspaceID 是所属工作区。
	WorkspaceID string
	// Prompt 是用户问题原文。
	Prompt string
	// Action 是将执行的动作，例如 export_data。
	Action string
	// Params 是规范化前的参数快照 JSON。
	Params []byte
	// ParamsHash 是规范化 JSON 的 SHA-256，小写十六进制。
	ParamsHash string
	// Sensitive 是涉及的敏感字段。
	Sensitive []string
	// EstimatedRows 是预估行数或影响范围。没有估计时为 0。
	EstimatedRows int64
	// Risk 是给用户看的风险说明。
	Risk string
	// StepID 是当前步骤。调用方传入，状态机不会从别的字段反推。
	StepID string
	// ExportObject 是导出对象。不是导出确认时可以空着。
	ExportObject string
	// ExpiresAt 是确认单失效时间。零值时创建逻辑用 24 小时。
	ExpiresAt time.Time
}

// Decision 是 Approver 的返回。
type Decision struct {
	Action Action
	// Params 为 nil 或空切片时沿用确认单里的快照。
	// 非空内容只出现在批准里，表示脚本或调用方给出了另一份参数。
	Params []byte
}

// Approver 在确认单已经落库之后征求一次决定。
//
// Request 在做出决定前阻塞。ctx 结束表示调用方不再等待（评测里的「不响应」，或状态机的确认超时）。
// 暂缓不是错误。重复提交和相反决定的幂等由状态机处理，不在这个接口里。
type Approver interface {
	Request(ctx context.Context, ticket Ticket) (Decision, error)
}

// ErrInvalidTicket 表示确认单缺少接口要求的字段。
var ErrInvalidTicket = errors.New("approval: 确认单不合法")

// ErrInvalidDecision 表示决定不是批准、拒绝或暂缓。
var ErrInvalidDecision = errors.New("approval: 决定不合法")

// ErrNoScript 表示 Fake 的预设已经用完。
var ErrNoScript = errors.New("approval: fake 没有预设决定")

// ErrSnapshotMismatch 表示参数和记录的 SHA-256 不一致。
// 状态机应拒绝执行并把运行标为 failed。它不是沙箱错误码。
var ErrSnapshotMismatch = errors.New("approval: 参数快照哈希不一致")

// Validate 检查确认单有没有标识、动作、参数和 64 位十六进制哈希。
// 它不核对哈希是否和参数匹配，那是 VerifySnapshot 的事。
func (t Ticket) Validate() error {
	if t.ID == "" || t.RunID == "" || t.WorkspaceID == "" || t.Action == "" {
		return fmt.Errorf("%w: 缺少标识或动作", ErrInvalidTicket)
	}
	if len(t.Params) == 0 || !validHash(t.ParamsHash) {
		return fmt.Errorf("%w: 缺少参数快照或哈希", ErrInvalidTicket)
	}
	return nil
}

// VerifySnapshot 在结构合法的前提下核对参数和哈希。
func (t Ticket) VerifySnapshot() error {
	if err := t.Validate(); err != nil {
		return err
	}
	ok, err := SnapshotMatches(t.ParamsHash, t.Params)
	if err != nil {
		return err
	}
	if !ok {
		return ErrSnapshotMismatch
	}
	return nil
}

// Validate 检查决定。只有批准可以携带替换参数。
// nil 和空切片都表示沿用确认单快照，不算替换。
func (d Decision) Validate() error {
	switch d.Action {
	case ActionApprove, ActionReject, ActionDefer:
	default:
		return fmt.Errorf("%w: 未知决定 %q", ErrInvalidDecision, d.Action)
	}
	if d.Action != ActionApprove && len(d.Params) > 0 {
		return fmt.Errorf("%w: 只有批准可以携带替换参数", ErrInvalidDecision)
	}
	return nil
}

// OverridesSnapshot 报告批准是否带了另一份参数。
// 空切片与 nil 同样表示没有替换，避免把没有内容的切片当成新快照。
func (d Decision) OverridesSnapshot() bool {
	return d.Action == ActionApprove && len(d.Params) > 0
}

func validHash(h string) bool {
	if len(h) != 64 {
		return false
	}
	_, err := hex.DecodeString(h)
	return err == nil
}
