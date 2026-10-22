//go:build linux

package agent

import (
	"strings"
	"testing"
	"time"

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
	// 这里验证真实 worker 的内存上限，不验证超时。通用单测 harness 的
	// 1 秒单步预算在全量检查争用 CPU 时会先超时，掩盖需要验证的资源错误。
	h.loop.Config.StepTimeout = 20 * time.Second
	h.loop.Config.TotalTimeout = 30 * time.Second
	out, err := h.run()
	// 计入失败会停在第3步，清零会继续到第5步；预期恰好停在第4步。
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
