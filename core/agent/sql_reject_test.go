package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/loveRyujin/lantern/core/llm"
	"github.com/loveRyujin/lantern/core/sqlx"
	"github.com/loveRyujin/lantern/core/tools"
	"github.com/loveRyujin/lantern/core/workspace"
)

func TestLoopSQLRejectedDoesNotRerunOrTouchFailureCount(t *testing.T) {
	t.Run("不计入也不清零", func(t *testing.T) {
		// 两次参数失败之后，run_sql 拒绝写语句。它既不计入连续失败，也不把计数清零。
		// 紧接着的参数失败是第三次，运行结束。同一条 SQL 即使把重试次数调到 3 也只执行一次。
		h, sqlTool := newSQLHarness(t,
			badStep("b1"),
			badStep("b2"),
			sqlStep("w", "DELETE FROM users"),
			badStep("b3"),
			llm.Say("不该走到这里"),
		)
		out, err := h.run()
		if out.ModelCalls != 4 || h.provider.n() != 4 {
			t.Fatalf("模型调用 %d/%d，想要 4。计入会停在 3，清零会走到最终回答", out.ModelCalls, h.provider.n())
		}
		if sqlTool.n() != 1 || h.echo.n() != 0 {
			t.Fatalf("run_sql %d 次，echo %d 次", sqlTool.n(), h.echo.n())
		}
		if !strings.Contains(sqlTool.query(0), "DELETE FROM users") {
			t.Fatalf("执行的 SQL = %s", sqlTool.query(0))
		}
		if out.State != StateFailed || !retryable(err) || errorCode(err) != tools.CodeArgumentsInvalid {
			t.Fatalf("结果 = %+v err=%v", out, err)
		}
		if strings.Contains(out.Text, "不该走到这里") {
			t.Fatal("sql_rejected 把连续计数清零了")
		}
		feedback := requestText(h.provider.requests()[3])
		if !strings.Contains(feedback, sqlx.CodeRejected) {
			t.Fatalf("拒绝没有作为工具错误回到模型: %s", feedback)
		}
	})

	t.Run("交回模型后可以改写", func(t *testing.T) {
		h, sqlTool := newSQLHarness(t,
			sqlStep("w", "DELETE FROM users"),
			sqlStep("r", "SELECT 1"),
			llm.Say("完成"),
		)
		out, err := h.run()
		if err != nil || out.State != StateCompleted || out.ModelCalls != 3 {
			t.Fatalf("%+v %v", out, err)
		}
		if sqlTool.n() != 2 {
			t.Fatalf("执行次数 %d，同一条 SQL 被自动重跑了", sqlTool.n())
		}
		if !strings.Contains(sqlTool.query(0), "DELETE") || !strings.Contains(sqlTool.query(1), "SELECT 1") {
			t.Fatalf("两次 SQL = %s | %s", sqlTool.query(0), sqlTool.query(1))
		}
		seen := requestText(h.provider.requests()[1])
		if !strings.Contains(seen, sqlx.CodeRejected) {
			t.Fatalf("模型没看到拒绝: %s", seen)
		}
		if !strings.Contains(out.Text, "完成") {
			t.Fatalf("改写后的查询没有跑完: %q", out.Text)
		}
	})
}

func TestLoopSQLResultTooLargeDoesNotTouchFailureCount(t *testing.T) {
	t.Run("不计入也不清零", func(t *testing.T) {
		// 两次参数失败之后，单独一行超过 1 MiB。它既不计入连续失败，也不把计数清零。
		// 紧接着的参数失败是第三次，运行结束。同一条 SQL 即使把重试次数调到 3 也只执行一次。
		huge := "SELECT hex(zeroblob(" + strconv.Itoa(sqlx.MaxResultBytes) + "))"
		h, sqlTool := newSQLHarness(t,
			badStep("b1"),
			badStep("b2"),
			sqlStep("big", huge),
			badStep("b3"),
			llm.Say("不该走到这里"),
		)
		out, err := h.run()
		if out.ModelCalls != 4 || h.provider.n() != 4 {
			t.Fatalf("模型调用 %d/%d，想要 4。计入会停在 3，清零会走到最终回答", out.ModelCalls, h.provider.n())
		}
		if sqlTool.n() != 1 {
			t.Fatalf("run_sql %d 次，同一条 SQL 被自动重跑了", sqlTool.n())
		}
		if !strings.Contains(sqlTool.query(0), "zeroblob") {
			t.Fatalf("执行的 SQL = %s", sqlTool.query(0))
		}
		if out.State != StateFailed || !retryable(err) || errorCode(err) != tools.CodeArgumentsInvalid {
			t.Fatalf("结果 = %+v err=%v", out, err)
		}
		if strings.Contains(out.Text, "不该走到这里") {
			t.Fatal("sql_result_too_large 把连续计数清零了")
		}
		feedback := requestText(h.provider.requests()[3])
		if !strings.Contains(feedback, sqlx.CodeResultTooLarge) {
			t.Fatalf("超限没有作为工具错误回到模型: %s", feedback)
		}
		if !strings.Contains(feedback, "substr") || !strings.Contains(feedback, "少选几列") {
			t.Fatalf("模型没看到改写建议: %s", feedback)
		}
	})
}

func newSQLHarness(t *testing.T, steps ...llm.Step) (*harness, *countingTool) {
	t.Helper()
	h := newHarness(t, echoSpec(), steps...)
	ws, err := workspace.Create(t.Context(), t.TempDir(), "sql-loop")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	path := filepath.Join(ws.Dir, "analytics.db")
	db, err := sqlx.Open(t.Context(), path, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	inner, err := sqlx.NewTool(db, path, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	spec := inner.Spec()
	spec.MaxAttempts = 3
	wrapped := &countingTool{Tool: inner, spec: spec}
	if err := h.reg.Register(wrapped); err != nil {
		t.Fatal(err)
	}
	return h, wrapped
}

func sqlStep(id, query string) llm.Step {
	raw, err := json.Marshal(map[string]string{"sql": query})
	if err != nil {
		panic(err)
	}
	return llm.CallTools(llm.ToolCall{ID: id, Name: "run_sql", Arguments: string(raw)})
}

// countingTool 把重试次数调高，用来证明 sql_rejected 仍然只执行一次。
type countingTool struct {
	tools.Tool
	spec  tools.Spec
	calls []tools.Call
}

func (c *countingTool) Spec() tools.Spec { return c.spec }

func (c *countingTool) Execute(ctx context.Context, call tools.Call) (tools.Result, error) {
	c.calls = append(c.calls, call)
	return c.Tool.Execute(ctx, call)
}

func (c *countingTool) n() int { return len(c.calls) }

func (c *countingTool) query(i int) string {
	if i < 0 || i >= len(c.calls) {
		return ""
	}
	var payload struct {
		SQL string `json:"sql"`
	}
	_ = json.Unmarshal(c.calls[i].Args, &payload)
	return payload.SQL
}
