// Package events 定义 Core 发出的类型化事件流。
//
// Core 把 token、step、approval_request 和 done 送到 Go channel。
// 工具步骤的 step 载荷带工具名和参数哈希，不带参数原文。
// UI 自己的 goroutine 再去更新窗口；评测 CLI 直接把事件写入轨迹。
// 本包不 import mygo。mygo 的 Channel 只允许出现在 internal/ui。
package events
