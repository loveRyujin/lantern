//go:build linux && amd64

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/analysisdata"
	"github.com/loveRyujin/lantern/core/llm"
	"github.com/loveRyujin/lantern/core/sandbox"
	"github.com/loveRyujin/lantern/core/sqlx"
	"github.com/loveRyujin/lantern/core/tools"
	"github.com/loveRyujin/lantern/core/workspace"
)

// The model is scripted; both tool observations come from real execution.
// The echo callback is a test adapter for #42, not production run_python (#43).
func TestLoopRealSQLDataToSandboxObservation(t *testing.T) {
	ws, err := workspace.Create(t.Context(), t.TempDir(), "data-loop")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	root := filepath.Join(ws.Dir, "tmp")
	runner := sandbox.New(sandbox.Config{TempRoot: root})
	probe, err := runner.Run(t.Context(), sandbox.Spec{Code: "print('data-loop-ready')"})
	if errors.Is(err, sandbox.ErrSandboxUnavailable) || errors.Is(err, sandbox.ErrRuntimeUnavailable) {
		if os.Getenv("SANDBOX_REQUIRE_REAL") != "1" {
			t.Skipf("real sandbox unavailable: %v", err)
		}
	}
	if err != nil || probe.Stdout != "data-loop-ready\n" {
		t.Fatalf("real sandbox probe: result=%+v error=%v", probe, err)
	}
	dbPath := filepath.Join(ws.Dir, "analytics.db")
	db, err := sqlx.Open(t.Context(), dbPath, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sqlTool, err := sqlx.NewTool(db, dbPath, dbPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"sql": "SELECT 9007199254740993 AS n UNION ALL SELECT 7 UNION ALL SELECT 111", "limit": 2})
	h := newHarness(t, echoSpec(),
		llm.CallTools(llm.ToolCall{ID: "real-sql", Name: "run_sql", Arguments: string(args)}),
		goodStep("convert-observation", "consume-last-sql"),
		llm.Say("已完成分析"),
	)
	h.loop.Config.StepTimeout = 20 * time.Second
	h.loop.Config.TotalTimeout = 40 * time.Second
	var actualSQL string
	wrapped := h.add(sqlTool.Spec())
	wrapped.fn = func(ctx context.Context, call tools.Call) (tools.Result, error) {
		result, err := sqlTool.Execute(ctx, call)
		if err == nil {
			actualSQL = result.Content
		}
		return result, err
	}
	h.echo.fn = func(ctx context.Context, _ tools.Call) (tools.Result, error) {
		if actualSQL == "" {
			return tools.Result{}, errors.New("SQL observation was not produced")
		}
		inputs, err := analysisdata.FromToolJSON(ctx, actualSQL)
		if err != nil {
			return tools.Result{}, err
		}
		code := `import os,runpy,json
columns,rows,meta=runpy.run_path(os.environ['INPUT_DIR']+'/lantern_table.py')['load_table']()
value={'sum':sum(row[0] for row in rows),'row_count':len(rows),'truncated':meta['truncated']}
open(os.environ['OUTPUT_DIR']+'/analysis.json','w').write(json.dumps(value))
print('actual-table-consumed')
`
		result, err := runner.Run(ctx, sandbox.Spec{Code: code, Inputs: inputs})
		if err != nil {
			return tools.Result{}, err
		}
		if len(result.Artifacts) != 1 || result.Artifacts[0].Name != "analysis.json" || result.Stdout != "actual-table-consumed\n" {
			return tools.Result{}, errors.New("real sandbox did not deliver its result")
		}
		return tools.Result{Content: string(result.Artifacts[0].Data), Summary: "真实 SQL 表计算完成"}, nil
	}
	out, err := h.run()
	if err != nil || out.State != StateCompleted || out.ModelCalls != 3 || wrapped.n() != 1 || h.echo.n() != 1 {
		t.Fatalf("outcome=%+v error=%v SQL=%d adapter=%d", out, err, wrapped.n(), h.echo.n())
	}
	if feedback := requestText(h.provider.requests()[1]); !strings.Contains(feedback, "9007199254740993") || !strings.Contains(feedback, "truncated") {
		t.Fatalf("model missed real SQL observation: %s", feedback)
	}
	feedback := requestText(h.provider.requests()[2])
	if !strings.Contains(feedback, "9007199254741000") || !strings.Contains(feedback, `"row_count": 2`) || !strings.Contains(feedback, `"truncated": true`) {
		t.Fatalf("model missed exact computed result and truncation: %s", feedback)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("Agent chain left workspace intermediates: %v error=%v", entries, err)
	}
}
