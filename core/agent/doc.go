// Package agent 跑 Agent 循环：think → tool_call → observe，直到最终回答。
//
// 参数不完整、不是合法 JSON、不符合 Schema，或工具名不存在时，不执行工具，不生成确认单，也不写参数快照。
// 这些失败按整次运行连续计数，见 failure.go。未知工具的错误码仍是 tool_not_found。
// 工具执行失败不计入，也不清零。max_steps 或超时为负时，调用模型之前就失败。0 表示用默认值。
//
// 策略只拿到参数的副本。普通批准和确认单两条路径，都在执行前重算参数哈希。
// 对不上就不执行，错误码是 hash_mismatch，并写一条审计。审计写失败时不执行工具，运行记为 failed。
// 工具在单独的 goroutine 里跑。时限到了就返回超时；如果工具一直不返回，那个 goroutine 会留下来，不再碰循环的状态。
package agent
