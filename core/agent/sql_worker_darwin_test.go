//go:build darwin

package agent

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/llm"
	"github.com/loveRyujin/lantern/core/sqlx"
	"github.com/loveRyujin/lantern/core/tools"
)

func TestDarwinLoopSQLReadOnly(t *testing.T) {
	h, sqlTool := newSQLHarness(t,
		sqlStep("q", "SELECT 42 AS n"),
		llm.Say("完成"),
	)
	h.loop.Config.StepTimeout = 20 * time.Second
	h.loop.Config.TotalTimeout = 30 * time.Second
	out, err := h.run()
	if err != nil || out.State != StateCompleted || out.ModelCalls != 2 || sqlTool.n() != 1 {
		t.Fatalf("outcome=%+v calls=%d sql=%d error=%v", out, out.ModelCalls, sqlTool.n(), err)
	}
	if !strings.Contains(out.Text, "完成") {
		t.Fatalf("loop did not finish after the SQL result: %+v", out)
	}
	seen := requestText(h.provider.requests()[1])
	if !strings.Contains(seen, "42") {
		t.Fatalf("model did not observe the SQL row: %s", seen)
	}
}

func TestDarwinLoopSQLWorkerMemoryLimitDoesNotTouchFailureCount(t *testing.T) {
	const columns = 320
	parts := make([]string, columns)
	for i := range parts {
		parts[i] = fmt.Sprintf("hex(zeroblob(2000000)) AS c%d", i)
	}
	query := "WITH x AS MATERIALIZED (SELECT " + strings.Join(parts, ", ") + ") SELECT count(*) FROM x"
	h, sqlTool := newSQLHarness(t,
		badStep("b1"), badStep("b2"), sqlStep("resource-limit", query),
		badStep("b3"), llm.Say("不该走到这里"),
	)
	h.loop.Config.StepTimeout = 90 * time.Second
	h.loop.Config.TotalTimeout = 120 * time.Second
	out, err := h.run()
	if out.ModelCalls != 4 || h.provider.n() != 4 || sqlTool.n() != 1 {
		t.Fatalf("model calls=%d/%d, SQL calls=%d, outcome=%+v error=%v", out.ModelCalls, h.provider.n(), sqlTool.n(), out, err)
	}
	if out.State != StateFailed || errorCode(err) != tools.CodeArgumentsInvalid {
		t.Fatalf("outcome=%+v error=%v", out, err)
	}
	if feedback := requestText(h.provider.requests()[3]); !strings.Contains(feedback, sqlx.CodeResultTooLarge) {
		t.Fatalf("worker memory error not returned to model: %s", feedback)
	}
}
