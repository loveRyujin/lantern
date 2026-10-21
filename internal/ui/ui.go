// Package ui 是桌面端的薄封装，也是仓库里唯一允许 import github.com/egoist/mygo 的包。
package ui

import "github.com/egoist/mygo"

// ToolkitVersion 返回链接进来的 mygo 版本。
func ToolkitVersion() string {
	return mygo.Version
}
