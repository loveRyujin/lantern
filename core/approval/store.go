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
// 只有状态机能改审批决定。Agent 在 Create 并征求决定之后，
// 如果存储同时实现了 Decider，就调用 Decide。内存假存储可以只实现 Store。
type Decider interface {
	Decide(ctx context.Context, id string, decision Decision) error
}
