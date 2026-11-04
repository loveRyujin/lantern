// Package ui 是桌面端的薄封装，也是仓库里唯一允许 import github.com/egoist/mygo 的包。
//
// 产品窗口是 [Run]。它列出工作区，用 markdown.Stream 画流式回复，
// 并且只通过 core/events 的事件更新界面。Core 不引用 mygo 的类型。
// [RunSpike] 仍是 W1 的最小探针，cmd/lantern 不进入那里。
package ui

import "github.com/egoist/mygo"

// ToolkitVersion 返回链接进来的 mygo 版本。
func ToolkitVersion() string {
	return mygo.Version
}
