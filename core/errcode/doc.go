// Package errcode 定义 Core 对外的稳定错误码。
//
// Agent、桌面端和评测 CLI 按错误码分支，不解析 Error() 文案。
// 文案可以改，码值是协议的一部分，改码值算破坏性变更。
// 带码错误实现 Coded，调用方用 CodeOf 或 errors.Is 识别。
package errcode
