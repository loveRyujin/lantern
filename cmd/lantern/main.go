// Command lantern 是 mygo 桌面端入口。
// 界面只经过 internal/ui，这里不直接 import mygo。
package main

import (
	"fmt"

	"github.com/loveRyujin/lantern/internal/ui"
)

func main() {
	fmt.Printf("TODO: lantern desktop (mygo %s)\n", ui.ToolkitVersion())
}
