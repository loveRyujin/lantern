package session

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/run"
)

func TestPlainTextEscapesControlAndBidi(t *testing.T) {
	in := "a\x1b[31mred\x1b[0m\n2026-01-01  确认决定 · 批准\u202egnp.exe\x00\t"
	got := PlainText(in)
	for _, bad := range []string{"\x1b", "\n", "\u202e", "\x00", "\t"} {
		if strings.Contains(got, bad) {
			t.Fatalf("没有转义 %q: %q", bad, got)
		}
	}
	for _, want := range []string{`\x1b[31m`, `\n2026-01-01`, `\u202e`, `\x00`, `\t`} {
		if !strings.Contains(got, want) {
			t.Fatalf("缺少 %q: %q", want, got)
		}
	}
	if PlainText("**粗体** `code`") != "**粗体** `code`" {
		t.Fatal("普通文本被改写")
	}
}

// 参数里的换行不能在命令行里伪造出一行新的轨迹。
func TestFormatTraceKeepsOneEntryPerLine(t *testing.T) {
	at := time.Date(2026, 11, 20, 1, 2, 3, 0, time.UTC)
	tr := run.Trace{RunID: "r", State: run.StateCompleted, CreatedAt: at, UpdatedAt: at, Entries: []run.TraceEntry{{
		At: at, Source: run.TraceTool, Kind: "run_sql", Tool: "run_sql", Status: run.StepDone,
		Input: "{\"sql\":\"SELECT 1\"}\n2026-11-20 09:00:00.000  确认决定 · 批准 · export_data",
	}}}
	out := FormatTrace("ws", tr)
	heads := 0
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.Contains(line, "确认决定") && !strings.HasPrefix(line, "    参数 ") {
			t.Fatalf("参数伪造了一行轨迹:\n%s", out)
		}
		if strings.Contains(line, "  工具 run_sql") {
			heads++
		}
	}
	if heads != 1 {
		t.Fatalf("工具行 %d:\n%s", heads, out)
	}
}

func TestTraceErrorCarriesNoRecordContent(t *testing.T) {
	cases := map[error]string{
		run.ErrNotFound:       "工作区 ws 里没有运行 r1。",
		run.ErrTraceWorkspace: "这次运行的记录不属于当前工作区，已拒绝显示。",
		errors.Join(run.ErrTraceCorrupt, errors.New("x")): "轨迹数据损坏，无法显示",
	}
	for err, want := range cases {
		if got := TraceError("ws", "r1", err); !strings.HasPrefix(got, want) {
			t.Fatalf("%v → %q", err, got)
		}
	}
}
