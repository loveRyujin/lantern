module github.com/loveRyujin/lantern

go 1.27.1

require (
	// Darwin spike libproc bindings without cgo; keep the existing version.
	github.com/ebitengine/purego v0.11.1
	// v0.6.0 = commit 081639f2a55d62a09aca742684827937dac6cb39。8 周内不主动升级。
	github.com/egoist/mygo v0.6.0
	// 工具参数的 JSON Schema 校验。
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2
	go.yaml.in/yaml/v3 v3.0.5
	// 只给 internal/seed 的调用方扫描测试用。
	golang.org/x/tools v0.52.0
	// Linux sandbox capability spike; pure Go kernel UAPI.
	golang.org/x/sys v0.49.0
	modernc.org/libc v1.77.1
	modernc.org/sqlite v1.60.1
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/go-text/typesetting v0.3.5 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/image v0.46.0 // indirect
	golang.org/x/mod v0.42.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)
