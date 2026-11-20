package ui

import (
	"context"
	"strings"
	"sync"
	"testing"

	mygoui "github.com/egoist/mygo/ui"
	"github.com/loveRyujin/lantern/core/agent"
	"github.com/loveRyujin/lantern/core/events"
	"github.com/loveRyujin/lantern/core/llm"
	"github.com/loveRyujin/lantern/core/pyrt"
	"github.com/loveRyujin/lantern/core/pythonx"
	"github.com/loveRyujin/lantern/core/tools"
	"github.com/loveRyujin/lantern/internal/seed"
	"github.com/loveRyujin/lantern/internal/ui/session"
)

// 没有准备 Python 时，真实 run_python 返回 runtime_unavailable。
// 回答下只出现分类后的错误，不出现图表；run_sql 真的读了种子库。
func TestChartRuntimeUnavailableShowsClassifiedFailure(t *testing.T) {
	h := newChartHarness(t, pyrt.Runtime{})
	tt := h.tt
	ask(t, tt, session.ChartDemoPrompt)
	tt.Frame()
	runID := h.lastRun(t)
	want := "Python 失败 · Python 环境不可用（runtime_unavailable）：Python 环境不可用，请先显式准备运行环境"
	if !tt.HasText(want) {
		t.Fatalf("缺少分类错误，现有 %q", tt.Texts())
	}
	if hasPrefixText(tt, "图表 ") {
		t.Fatalf("失败的运行画出了图表：%q", tt.Texts())
	}
	rc, ok := h.an.Charts("星禾", runID)
	if !ok || len(rc.Charts) != 0 || len(rc.Failures) != 1 || rc.Failures[0].CallID != "py-"+runID || rc.Failures[0].Code != "runtime_unavailable" {
		t.Fatalf("记录=%+v ok=%v", rc, ok)
	}
	sqlSeen := h.rec.observation(t, "sql-"+runID)
	if !strings.Contains(sqlSeen, "2026-09") || !strings.Contains(sqlSeen, "12104587") {
		t.Fatalf("run_sql 没有读到种子库的华东 9 月金额：%s", sqlSeen)
	}
	if msg := h.s.state.Thread("星禾").Messages[1]; msg.Status != events.DoneCompleted {
		t.Fatalf("status=%s", msg.Status)
	}

	if _, err := h.an.Fetch("星禾", runID, "missing"); code(err) != pythonx.CodeArtifact {
		t.Fatalf("缺失产物 err=%v", err)
	}
	if _, err := h.an.Fetch("云杉", runID, "missing"); code(err) != pythonx.CodeScope {
		t.Fatalf("跨区取图 err=%v", err)
	}
	if _, ok := h.an.Charts("云杉", runID); ok {
		t.Fatal("别的工作区拿到了这次运行的记录")
	}
	if err := tt.Click("云杉"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if tt.HasText(want) {
		t.Fatal("切到云杉还能看见星禾的失败")
	}
	if err := h.an.CloseRun(runID); err != nil {
		t.Fatal(err)
	}
	if rc, _ := h.an.Charts("星禾", runID); !rc.Closed {
		t.Fatal("CloseRun 后记录没有标成已关闭")
	}
}

type chartHarness struct {
	s    *shell
	tt   *mygoui.Tester
	lr   *session.LoopRunner
	an   *session.Analysis
	rec  *recordingProviders
	root string
}

func newChartHarness(t *testing.T, rt pyrt.Runtime) *chartHarness {
	t.Helper()
	root := t.TempDir()
	if _, err := seed.Run(t.Context(), seed.Options{Root: root, Seed: seed.DefaultSeed}); err != nil {
		t.Fatal(err)
	}
	an := &session.Analysis{Runtime: rt}
	t.Cleanup(func() { _ = an.Close() })
	lr := session.NewChartDemoRunner(root, session.NewWaiter(), an)
	rec := &recordingProviders{}
	rec.wrap(lr)
	s, err := newShell(t.Context(), Options{
		Root:       root,
		Workspaces: []session.Workspace{{ID: "星禾"}, {ID: "云杉"}},
		Runner:     lr,
	})
	if err != nil {
		t.Fatal(err)
	}
	tt := tester(s)
	tt.Frame()
	return &chartHarness{s: s, tt: tt, lr: lr, an: an, rec: rec, root: root}
}

func (h *chartHarness) lastRun(t *testing.T) string {
	t.Helper()
	m := latestAssistant(h.s.state)
	if m == nil || !m.Done {
		t.Fatalf("没有完成的回答：%+v", m)
	}
	return m.RunID
}

// recordingProviders 记下每次运行的模型请求，用来核对模型看到的真实工具观察值。
type recordingProviders struct {
	mu   sync.Mutex
	reqs []llm.Request
}

func (r *recordingProviders) wrap(lr *session.LoopRunner) {
	next := lr.NewProvider
	lr.NewProvider = func(runID string) llm.Provider { return recordingProvider{r, next(runID)} }
}

func (r *recordingProviders) observation(t *testing.T, callID string) string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, req := range r.reqs {
		for _, m := range req.Messages {
			if m.ToolCallID == callID {
				return m.Content
			}
		}
	}
	t.Fatalf("模型没有收到 %s 的观察值", callID)
	return ""
}

type recordingProvider struct {
	r     *recordingProviders
	inner llm.Provider
}

func (p recordingProvider) ID() string { return p.inner.ID() }

func (p recordingProvider) Chat(ctx context.Context, req llm.Request) (llm.Response, error) {
	p.r.mu.Lock()
	p.r.reqs = append(p.r.reqs, req)
	p.r.mu.Unlock()
	return p.inner.Chat(ctx, req)
}

func code(err error) string {
	c, _ := tools.CodeOf(err)
	return c
}

func hasPrefixText(tt *mygoui.Tester, prefix string) bool {
	for _, s := range tt.Texts() {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

func observationInner(t *testing.T, source, wrapped string) string {
	t.Helper()
	open := strings.Replace(agent.DataBoundaryOpen, "%s", source, 1)
	if !strings.HasPrefix(wrapped, open) || !strings.HasSuffix(wrapped, agent.DataBoundaryClose) {
		t.Fatalf("观察值没有数据边界：%s", wrapped)
	}
	return strings.TrimSuffix(strings.TrimPrefix(wrapped, open), agent.DataBoundaryClose)
}

// 回答和图表变长后提问框仍留在窗口里，可以继续提问。
func TestLongConversationKeepsInputInWindow(t *testing.T) {
	long := strings.Repeat("一行结论\n\n", 80)
	s := testShell(t, session.ScriptedRunner{Answer: func(string, string) string { return long }}, "星禾")
	tt := tester(s)
	tt.Frame()
	ask(t, tt, "第一问")
	tt.Frame()
	r, ok := tt.Find("提问框")
	if !ok || r.Y+r.H > 720 {
		t.Fatalf("提问框在窗口外：%+v", r)
	}
	ask(t, tt, "第二问")
	tt.Frame()
	if n := len(s.state.Thread("星禾").Messages); n != 4 {
		t.Fatalf("第二问没有发出去，消息数 %d", n)
	}
}
