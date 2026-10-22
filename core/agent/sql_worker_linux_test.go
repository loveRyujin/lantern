//go:build linux

package agent

import (
	"strings"
	"testing"

	"github.com/loveRyujin/lantern/core/llm"
	"github.com/loveRyujin/lantern/core/sqlx"
	"github.com/loveRyujin/lantern/core/tools"
)

func TestLoopSQLWorkerMemoryLimitDoesNotTouchFailureCount(t *testing.T) {
	cols := strings.TrimSuffix(strings.Repeat("hex(zeroblob(2000000)),", 200), ",")
	query := "WITH x AS MATERIALIZED (SELECT " + cols + ") SELECT count(*) FROM x"
	h, sqlTool := newSQLHarness(t,
		badStep("b1"), badStep("b2"), sqlStep("resource-limit", query),
		badStep("b3"), llm.Say("不该走到这里"),
	)
	out, err := h.run()
	// 计入失败会停在第3步，清零会继续到第5步；预期恰好停在第4步。
	if out.ModelCalls != 4 || h.provider.n() != 4 || sqlTool.n() != 1 {
		t.Fatalf("model calls=%d/%d, SQL calls=%d", out.ModelCalls, h.provider.n(), sqlTool.n())
	}
	if out.State != StateFailed || errorCode(err) != tools.CodeArgumentsInvalid {
		t.Fatalf("outcome=%+v error=%v", out, err)
	}
	if feedback := requestText(h.provider.requests()[3]); !strings.Contains(feedback, sqlx.CodeResultTooLarge) {
		t.Fatalf("worker memory error not returned to model: %s", feedback)
	}
}
