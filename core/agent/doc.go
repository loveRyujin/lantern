// Package agent 跑 Agent 循环：think → tool_call → observe，直到最终回答。
//
// 参数不完整、不是合法 JSON、不符合 Schema，或工具名不存在时，不执行工具，不生成确认单，也不写参数快照。
// 这些失败按整次运行连续计数，见 failure.go。未知工具的错误码仍是 tool_not_found。
// 工具执行失败不计入，也不清零。max_steps 或超时为负时，调用模型之前就失败。0 表示用默认值。
package agent
