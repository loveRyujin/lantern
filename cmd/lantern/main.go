// Command lantern 是桌面端入口。
//
// 界面只经过 internal/ui。这里不 import mygo，Core 也不 import mygo。
//
//	CGO_ENABLED=0 go run ./cmd/lantern
//	CGO_ENABLED=0 go run ./cmd/lantern -root /tmp/lantern-demo -check -shot /tmp/lantern-window.png
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/loveRyujin/lantern/internal/ui"
)

func main() {
	root := flag.String("root", "", "应用数据目录。空则用本机 Lantern 目录")
	check := flag.Bool("check", false, "打开窗口，自动提一个问题，看到流式 Markdown 后退出")
	shot := flag.String("shot", "", "和 -check 一起用，把结束时的窗口写成 PNG")
	flag.Parse()
	opts := ui.Options{Root: *root, CheckShot: *shot}
	if *check {
		opts.CheckPrompt = "上个月营收是多少"
	}
	if err := ui.Run(opts); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
