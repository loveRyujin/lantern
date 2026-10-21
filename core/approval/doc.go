// Package approval 定义 Approver 接口、确认单、决定和参数快照哈希。
//
// 确认单先落库，再调用 Approver。UI 弹窗和评测脚本都实现这个接口。
// Escape 表示暂缓，返回 Defer 且 error 为 nil，运行保持 awaiting_approval。
// 不响应由调用方取消 ctx；本包不把这种等待记成沙箱的 timeout 错误码。
// 参数是否被篡改，由执行前重新计算的 SHA-256 决定，Approver 不直接执行工具。
//
// Store 是确认单的落库口。状态机实现它。调用方先 Create，再 Request。
// 参数还没通过校验时不要 Create，避免为残缺参数生成确认单。
package approval
