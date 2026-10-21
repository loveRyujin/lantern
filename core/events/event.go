package events

import (
	"fmt"
	"time"
)

// Kind 是事件类型。码值是协议的一部分。
type Kind string

const (
	// KindToken 是模型输出的一段文本。
	KindToken Kind = "token"
	// KindStep 是运行步骤的状态变化。
	KindStep Kind = "step"
	// KindApprovalRequest 表示确认单已经落库，UI 或脚本可以去处理。
	KindApprovalRequest Kind = "approval_request"
	// KindDone 表示这次运行到达终态。
	KindDone Kind = "done"
)

const (
	// StepExecuting 表示工具或模型步骤已经开始，并且已经落成 executing。
	StepExecuting = "executing"
	// StepDone 表示步骤正常结束。
	StepDone = "done"
	// StepError 表示步骤以错误结束。
	StepError = "error"
)

const (
	// DoneCompleted 表示运行正常完成。
	DoneCompleted = "completed"
	// DoneFailed 表示运行失败。
	DoneFailed = "failed"
	// DoneCancelled 表示运行被取消。
	DoneCancelled = "cancelled"
	// DoneRejected 表示确认被拒绝。
	DoneRejected = "rejected"
	// DoneExpired 表示确认等待超时。
	DoneExpired = "expired"
)

// Event 是事件流上的一条记录。
//
// 四种 Kind 各自只携带对应的那一个载荷，别的指针必须是 nil。
// Seq 和零值 Time 由 Publisher 填写。调用方预设的非零 Time 会保留。
type Event struct {
	// Kind 是事件类型。
	Kind Kind
	// RunID 是所属运行，必填。
	RunID string
	// Time 为零时由 Publisher 填当前时间。
	Time time.Time
	// Seq 由 Publisher 按送达顺序从 1 开始填写。
	Seq uint64
	// Token 只在 KindToken 时非 nil。
	Token *Token
	// Step 只在 KindStep 时非 nil。
	Step *Step
	// Approval 只在 KindApprovalRequest 时非 nil。
	Approval *ApprovalRequest
	// Done 只在 KindDone 时非 nil。
	Done *Done
}

// Token 是一段流式文本。Text 不能为空。
type Token struct {
	// Text 是这段输出，不能为空。
	Text string
}

// Step 是步骤摘要。ID 和 Status 必填。
type Step struct {
	// ID 是步骤标识。
	ID string
	// Status 是 executing、done 或 error。
	Status string
	// Summary 是输入、工具或结果的摘要。
	Summary string
	// Tokens 是这一步消耗的 token 数，没有时为 0。
	Tokens int
	// Duration 是这一步的耗时。
	Duration time.Duration
}

// ApprovalRequest 只携带确认单的关联信息。
// 参数正文留在已经落库的确认单里，避免事件流再复制一份敏感内容。
type ApprovalRequest struct {
	// TicketID 用来回到已经落库的确认单。
	TicketID string
	// Action 是将执行的动作。
	Action string
	// ParamsHash 是参数快照哈希，便于和确认单对照。
	ParamsHash string
}

// Done 是运行终态。Text 是最终回答，失败时可以是空的。
type Done struct {
	// Status 是 completed、failed、cancelled、rejected 或 expired。
	Status string
	// Text 是最终回答，失败时可以是空的。
	Text string
}

// Validate 检查类型和载荷是否一一对应。
func (e Event) Validate() error {
	if e.RunID == "" {
		return fmt.Errorf("events: 缺少 run_id")
	}
	n := 0
	if e.Token != nil {
		n++
	}
	if e.Step != nil {
		n++
	}
	if e.Approval != nil {
		n++
	}
	if e.Done != nil {
		n++
	}
	switch e.Kind {
	case KindToken:
		if n != 1 || e.Token == nil || e.Token.Text == "" {
			return fmt.Errorf("events: token 事件不合法")
		}
	case KindStep:
		if n != 1 || e.Step == nil || e.Step.ID == "" || !validStep(e.Step.Status) {
			return fmt.Errorf("events: step 事件不合法")
		}
	case KindApprovalRequest:
		if n != 1 || e.Approval == nil || e.Approval.TicketID == "" {
			return fmt.Errorf("events: approval_request 事件不合法")
		}
	case KindDone:
		if n != 1 || e.Done == nil || !validDone(e.Done.Status) {
			return fmt.Errorf("events: done 事件不合法")
		}
	default:
		return fmt.Errorf("events: 未知类型 %q", e.Kind)
	}
	return nil
}

func validStep(status string) bool {
	switch status {
	case StepExecuting, StepDone, StepError:
		return true
	default:
		return false
	}
}

func validDone(status string) bool {
	switch status {
	case DoneCompleted, DoneFailed, DoneCancelled, DoneRejected, DoneExpired:
		return true
	default:
		return false
	}
}

func prepare(ev Event, now time.Time) (Event, error) {
	if err := ev.Validate(); err != nil {
		return Event{}, err
	}
	ev = cloneEvent(ev)
	if ev.Time.IsZero() {
		ev.Time = now
	}
	ev.Seq = 0
	return ev, nil
}

func cloneEvent(ev Event) Event {
	if ev.Token != nil {
		token := *ev.Token
		ev.Token = &token
	}
	if ev.Step != nil {
		step := *ev.Step
		ev.Step = &step
	}
	if ev.Approval != nil {
		approval := *ev.Approval
		ev.Approval = &approval
	}
	if ev.Done != nil {
		done := *ev.Done
		ev.Done = &done
	}
	return ev
}
