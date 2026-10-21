package approval

import "context"

// Store 把已经通过参数校验的确认单落库。
//
// 审批状态机（确认单的创建、决定和消费）实现这个接口。
// Agent 只在完整性、JSON 和 Schema 都通过之后调用 Create，
// 然后才把同一张确认单交给 Approver。本接口不执行工具，也不写决定。
type Store interface {
	Create(ctx context.Context, ticket Ticket) error
}

// Decider 把 Approver 返回的决定写回确认单。
// 只有状态机能改审批决定。Agent 在批准或拒绝时必须拿到 Decider，
// 存储没实现这个接口就直接失败，不会静默跳过再去执行工具。
type Decider interface {
	Decide(ctx context.Context, id string, decision Decision) error
}

// Consumer 在执行前把已批准的确认单消费掉。
// 调用方传入当前运行、步骤和参数哈希。条件更新成功之后才能执行。
type Consumer interface {
	Consume(ctx context.Context, id string, cc ConsumeContext) error
}
