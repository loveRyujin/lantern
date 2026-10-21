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
