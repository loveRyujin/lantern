// Command lantern 是桌面端入口。
//
// 界面只经过 internal/ui。这里不 import mygo，Core 也不 import mygo。
//
//	CGO_ENABLED=0 go run ./cmd/lantern
//	CGO_ENABLED=0 go run ./cmd/lantern -root /tmp/lantern-demo -check -shot /tmp/lantern-window.png
//	CGO_ENABLED=0 go run ./cmd/lantern -root /tmp/lantern-demo -workspace 星禾 -trace ui-0123…
//
// 每次启动先对每个工作区跑一次 Recover（启动扫描）。有提问、确认单或恢复执行在进行时，
// 关窗口或第一个 SIGINT/SIGTERM 会弹拦截框；第二个信号直接退出，留给下次启动扫描。
//
// -pyrt-root 是显式的可信准备：首次会建 venv 并按哈希锁安装 pandas、matplotlib。
// 准备好后窗口改用场景 1 出图演示，工作区需要先 make seed。
//
//	CGO_ENABLED=0 go run ./cmd/lantern -root /tmp/lantern-demo -pyrt-root /tmp/lantern-pyrt -check -shot /tmp/lantern-chart.png
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/loveRyujin/lantern/core/pyrt"
	"github.com/loveRyujin/lantern/internal/ui"
	"github.com/loveRyujin/lantern/internal/ui/session"
)

func main() {
	root := flag.String("root", "", "应用数据目录。空则用本机 Lantern 目录")
	check := flag.Bool("check", false, "打开窗口，自动提问，截到确认弹窗后暂缓并退出")
	shot := flag.String("shot", "", "和 -check 一起用，把结束时的窗口写成 PNG")
	pyRoot := flag.String("pyrt-root", "", "准备 Python 运行时的目录。设置后窗口演示场景 1 出图")
	chartDemo := flag.Bool("chart-demo", false, "不准备 Python，窗口演示场景 1 的结构化图表；run_python 会报 Python 环境不可用")
	trace := flag.String("trace", "", "只读打印这次运行的轨迹后退出，不打开窗口")
	ws := flag.String("workspace", "", "和 -trace 一起用，运行所在的工作区")
	toolDelay := flag.Duration("demo-tool-delay", 0, "演示 export_data 批准后先等这么久再执行，用来验证工具执行中退出")
	flag.Parse()
	if *trace != "" {
		if err := ui.WriteTrace(context.Background(), os.Stdout, *root, *ws, *trace); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	opts := ui.Options{Root: *root, CheckShot: *shot, DemoToolDelay: *toolDelay}
	if *check {
		opts.CheckPrompt = "上个月营收是多少"
	}
	if *pyRoot != "" {
		rt, err := (pyrt.Config{Root: *pyRoot}).Ensure(context.Background())
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		opts.Python = &rt
	} else if *chartDemo {
		opts.Python = &pyrt.Runtime{}
	}
	if opts.Python != nil && *check {
		opts.CheckPrompt = session.ChartDemoPrompt
	}
	if err := ui.Run(opts); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
