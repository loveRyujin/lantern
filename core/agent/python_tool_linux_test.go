//go:build linux && amd64

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/llm"
	"github.com/loveRyujin/lantern/core/pyrt"
	"github.com/loveRyujin/lantern/core/pythonx"
	"github.com/loveRyujin/lantern/core/sandbox"
	"github.com/loveRyujin/lantern/core/tools"
	"github.com/loveRyujin/lantern/core/workspace"
)

func TestLoopProductionPythonRuntimeFailurePreservesCount(t *testing.T) {
	ws, err := workspace.Create(t.Context(), t.TempDir(), "python-counter")
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	s, err := pythonx.New(t.Context(), pythonx.Config{Workspace: ws, RunID: "python-counter"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := newHarness(t, echoSpec(), badStep("bad1"), badStep("bad2"),
		llm.CallTools(llm.ToolCall{ID: "python", Name: "run_python", Arguments: `{"code":"print('must-not-execute')"}`}), badStep("bad3"), llm.Say("unreachable"))
	h.loop.Config.StepTimeout = 20 * time.Second
	h.loop.Config.TotalTimeout = 40 * time.Second
	counted := h.add(s.PythonTool().Spec())
	counted.fn = s.PythonTool().Execute
	out, err := h.loop.Run(t.Context(), Input{RunID: "python-counter", WorkspaceID: ws.ID, Prompt: "执行分析"})
	if out.ModelCalls != 4 || counted.n() != 1 || out.State != StateFailed || errorCode(err) != tools.CodeArgumentsInvalid {
		t.Fatalf("runtime failure changed argument count: outcome=%+v error=%v calls=%d", out, err, counted.n())
	}
	if feedback := requestText(h.provider.requests()[3]); !strings.Contains(feedback, "runtime_unavailable") || strings.Contains(feedback, ws.Dir) {
		t.Fatalf("missing or path-leaking runtime feedback: %s", feedback)
	}
}

// The provider only scripts tool selection. The registered tools below are the
// production Session tools, including source lookup, prepared-runtime lease,
// the real Linux Runner and its artifact store; no echo execution adapter.
func TestLoopProductionPythonRealChart(t *testing.T) {
	if os.Getenv("SANDBOX_REAL_PACKAGES") != "1" {
		t.Skip("real prepared pandas/Agg production tool requires SANDBOX_REAL_PACKAGES=1")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Minute)
	defer cancel()
	rt, err := (pyrt.Config{Root: t.TempDir()}).Ensure(ctx)
	if err != nil {
		t.Fatalf("required real environment: %v", err)
	}
	ws, err := workspace.Create(ctx, t.TempDir(), "python-chart")
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	if _, err := ws.Analytics.ExecContext(ctx, "CREATE TABLE qa_points (x INTEGER,y INTEGER); INSERT INTO qa_points VALUES (1,2),(2,4),(3,6)"); err != nil {
		t.Fatal(err)
	}
	s, err := pythonx.New(ctx, pythonx.Config{Workspace: ws, RunID: "python-chart", Runtime: rt})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	code := `import os,pandas as pd,matplotlib
assert matplotlib.get_backend().lower()=='agg'
assert os.path.commonpath([os.environ['MPLCONFIGDIR'],os.getcwd()])==os.getcwd()
columns,rows,meta=load_table()
frame=pd.DataFrame(rows,columns=columns)
assert frame['y'].sum()==12
import matplotlib.pyplot as plt
fig,ax=plt.subplots()
ax.plot(frame['x'],frame['y'])
fig.savefig(os.environ['OUTPUT_DIR']+'/chart.png')
print('production-chart-sum:12')
`
	pythonArgs, _ := json.Marshal(map[string]string{"code": code, "sql_call_id": "query-points"})
	h := newHarness(t, echoSpec(), sqlStep("query-points", "SELECT x,y FROM qa_points ORDER BY x"),
		llm.CallTools(llm.ToolCall{ID: "plot-failure", Name: "run_python", Arguments: `{"code":"raise ValueError('controlled-chart-error')","sql_call_id":"query-points"}`}),
		llm.CallTools(llm.ToolCall{ID: "plot", Name: "run_python", Arguments: string(pythonArgs)}), llm.Say("图表已生成，合计12"))
	h.loop.Config.StepTimeout = 45 * time.Second
	h.loop.Config.TotalTimeout = 90 * time.Second
	if err := h.reg.Register(s.SQLTool()); err != nil {
		t.Fatal(err)
	}
	if err := h.reg.Register(s.PythonTool()); err != nil {
		t.Fatal(err)
	}
	out, err := h.loop.Run(ctx, Input{RunID: "python-chart", WorkspaceID: ws.ID, Prompt: "查询数据并生成趋势图"})
	if err != nil || out.State != StateCompleted || out.ModelCalls != 4 || h.echo.n() != 0 || len(h.approver.Tickets()) != 0 {
		t.Fatalf("production Agent chain: outcome=%+v error=%v", out, err)
	}
	var observation struct {
		Stdout    string             `json:"stdout"`
		Artifacts []pythonx.Manifest `json:"artifacts"`
	}
	found := false
	if feedback := requestText(h.provider.requests()[2]); !strings.Contains(feedback, "tools: error:") {
		t.Fatalf("model never observed the actual Python failure: %s", feedback)
	}
	for _, message := range h.provider.requests()[3].Messages {
		if message.ToolCallID == "plot" {
			found = true
			if err := json.Unmarshal([]byte(message.Content), &observation); err != nil {
				t.Fatalf("model did not receive manifest JSON: %s %v", message.Content, err)
			}
			if strings.Contains(message.Content, ws.Dir) || strings.Contains(message.Content, rt.Venv) || strings.Contains(message.Content, "base64") {
				t.Fatalf("model received bare path/payload: %s", message.Content)
			}
		}
	}
	if !found || observation.Stdout != "production-chart-sum:12\n" || len(observation.Artifacts) != 1 || observation.Artifacts[0].Name != "chart.png" {
		t.Fatalf("next model request missing actual chart result: %+v", observation)
	}
	artifact, err := s.GetArtifact(ws.ID, "python-chart", observation.Artifacts[0].ID)
	if err != nil || artifact.Kind != sandbox.KindPNG || len(artifact.Data) != observation.Artifacts[0].Bytes || sandbox.ValidateArtifact(artifact, 0) != nil {
		t.Fatalf("Agent.Run finished without retrievable chart: name=%s bytes=%d error=%v", artifact.Name, len(artifact.Data), err)
	}
	if _, err := png.Decode(bytes.NewReader(artifact.Data)); err != nil {
		t.Fatalf("returned chart is not a decoded PNG: %v", err)
	}
	if entries, err := os.ReadDir(filepath.Join(ws.Dir, "tmp")); err != nil || len(entries) != 0 {
		t.Fatalf("Agent chart intermediates remain: %v %v", entries, err)
	}
}
