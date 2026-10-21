// Package policy 决定一次工具调用要不要本人确认。
//
// 判定只看注册时的工具名，不看模型输出里的 requires_approval、approved
// 或其他字段。模型不能靠参数把自己标成已批准。
package policy
