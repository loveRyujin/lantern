// Command lantern 是桌面端入口。
//
// 界面只经过 internal/ui。这里不 import mygo，Core 也不 import mygo。
//
//	CGO_ENABLED=0 go run ./cmd/lantern
//	CGO_ENABLED=0 go run ./cmd/lantern -root /tmp/lantern-demo -check -shot /tmp/lantern-window.png
//	CGO_ENABLED=0 go run ./cmd/lantern -root /tmp/lantern-demo -workspace 星禾 -trace ui-0123…
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/loveRyujin/lantern/internal/ui"
)

func main() {
	root := flag.String("root", "", "应用数据目录。空则用本机 Lantern 目录")
	check := flag.Bool("check", false, "打开窗口，自动提问，截到确认弹窗后暂缓并退出")
	shot := flag.String("shot", "", "和 -check 一起用，把结束时的窗口写成 PNG")
	trace := flag.String("trace", "", "只读打印这次运行的轨迹后退出，不打开窗口")
	ws := flag.String("workspace", "", "和 -trace 一起用，运行所在的工作区")
	flag.Parse()
	if *trace != "" {
		if err := ui.WriteTrace(context.Background(), os.Stdout, *root, *ws, *trace); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	opts := ui.Options{Root: *root, CheckShot: *shot}
	if *check {
		opts.CheckPrompt = "上个月营收是多少"
	}
	if err := ui.Run(opts); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
