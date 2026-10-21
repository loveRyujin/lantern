// Package agent 跑 Agent 循环：think → tool_call → observe，直到最终回答。
//
// 参数不完整、不是合法 JSON 或不符合 Schema 时，不执行工具，不生成确认单，也不写参数快照。
// 连续参数失败按整次运行计数，见 failure.go。工具执行失败不计入，也不清零。
package agent
