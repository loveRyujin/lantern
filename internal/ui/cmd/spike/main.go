// Command spike 打开 internal/ui 里的 mygo 最小窗口。
//
//	CGO_ENABLED=0 go run ./internal/ui/cmd/spike
//	CGO_ENABLED=0 go run ./internal/ui/cmd/spike -shots docs/adr/img
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/loveRyujin/lantern/internal/ui"
)

func main() {
	shots := flag.String("shots", "", "把 stream.png、done.png、alert.png 写到这个目录后退出")
	flag.Parse()
	if err := ui.RunSpike(ui.SpikeOptions{ShotsDir: *shots}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
