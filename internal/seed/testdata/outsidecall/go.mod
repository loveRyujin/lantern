// 模块路径必须在 github.com/loveRyujin/lantern 下面，否则引用不了 internal/seed。
// 它不是 internal/seed 的子路径，扫描时应把这里的引用都报出来。
module github.com/loveRyujin/lantern/outsidecall

go 1.27.1

require github.com/loveRyujin/lantern v0.0.0

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	modernc.org/libc v1.77.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
	modernc.org/sqlite v1.60.1 // indirect
)

replace github.com/loveRyujin/lantern => ../../../..
