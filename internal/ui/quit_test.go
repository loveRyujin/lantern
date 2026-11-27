package ui

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/egoist/mygo"
	mygoui "github.com/egoist/mygo/ui"
	"github.com/loveRyujin/lantern/core/approval"
	"github.com/loveRyujin/lantern/core/llm"
	"github.com/loveRyujin/lantern/core/tools"
	"github.com/loveRyujin/lantern/core/workspace"
	"github.com/loveRyujin/lantern/internal/ui/session"
)

const quitTitle = "仍要退出 lantern？"

// quitRig 用真实 Core（session.LoopRunner：agent.Loop + run.Service）跑提问。
// start 模拟一次进程启动：新的等待器、运行器和 shell，newShell 里跑启动扫描。工具对象跨重启共用，计数就是总执行次数。
type quitRig struct {
	t           *testing.T
	root        string
	tool        tools.Tool
	newProvider func(string) llm.Provider
	grace       time.Duration

	s      *shell
	tt     *mygoui.Tester
	waiter *session.Waiter
	exited chan time.Time
}

func newQuitRig(t *testing.T, tool tools.Tool, np func(string) llm.Provider) *quitRig {
	t.Helper()
	root := t.TempDir()
	createWorkspaces(t, root, "星禾", "云杉")
	r := &quitRig{t: t, root: root, tool: tool, newProvider: np, grace: 2 * time.Second}
	r.start()
	return r
}

func createWorkspaces(t *testing.T, root string, ids ...string) {
	t.Helper()
	for _, id := range ids {
		h, err := workspace.Create(t.Context(), root, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func (r *quitRig) start() {
	r.t.Helper()
	r.waiter = session.NewWaiter()
	runner := &session.LoopRunner{Root: r.root, Waiter: r.waiter, Tool: r.tool, NewProvider: r.newProvider, Demo: true}
	s, err := newShell(r.t.Context(), Options{
		Root:       r.root,
		Workspaces: []session.Workspace{{ID: "星禾"}, {ID: "云杉"}},
		Runner:     runner,
	})
	if err != nil {
		r.t.Fatal(err)
	}
	// 测试线程就是界面线程：提问在后台跑，每个事件经 hops 在 Frame 里落到状态上。
	s.post = func(fn func()) { s.hops <- fn }
	s.grace = r.grace
	exited := make(chan time.Time, 1)
	s.exit = func() { exited <- time.Now() }
	r.s, r.exited = s, exited
	r.tt = tester(s)
	r.tt.Frame()
}

func (r *quitRig) pump(what string, cond func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r.tt.Frame()
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("等待超时：%s phase=%s notice=%q texts=%q", what, r.s.state.Phase(), shellNotice(r.s), r.tt.Texts())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (r *quitRig) click(label string) {
	r.t.Helper()
	if err := r.tt.Click(label); err != nil {
		r.t.Fatalf("点击 %q：%v，现有 %q", label, err, r.tt.Texts())
	}
}

// waitDialog 等确认框画出来，返回它展示的票据。
func (r *quitRig) waitDialog() approval.Ticket {
	r.t.Helper()
	var tk approval.Ticket
	r.pump("确认框", func() bool {
		r.s.mu.Lock()
		defer r.s.mu.Unlock()
		if r.s.asking && r.s.dialog != nil {
			tk = *r.s.dialog
		}
		return tk.ID != "" && r.tt.HasText("批准")
	})
	return tk
}

func (r *quitRig) idle() bool { return r.s.activity().empty() }

// interceptVia 触发一次退出回调，要求被拦住且拦截框画出来、下面的确认框不画。
func (r *quitRig) interceptVia(kind string) {
	r.t.Helper()
	if r.fire(kind) {
		r.t.Fatalf("%s 没有被拦住", kind)
	}
	r.pump("拦截框", func() bool {
		return r.tt.HasText(quitTitle) && r.tt.HasText("取消退出") && !r.tt.HasText("批准")
	})
}

// fire 按 mygo 的方式触发 OnClose 或 OnBeforeQuit，返回是否放行。
func (r *quitRig) fire(kind string) bool {
	switch kind {
	case "close":
		e := &mygo.CloseEvent{}
		r.s.onClose(e)
		return !e.DefaultPrevented()
	default:
		e := &mygo.QuitEvent{}
		r.s.onBeforeQuit(e)
		return !e.DefaultPrevented()
	}
}

// leaveAndWait 点「仍退出」，界面线程继续出帧，直到 exit 被调用。返回点击到退出的耗时。
func (r *quitRig) leaveAndWait() time.Duration {
	r.t.Helper()
	start := time.Now()
	r.click("仍退出")
	var at time.Time
	r.pump("退出", func() bool {
		select {
		case at = <-r.exited:
			return true
		default:
			return false
		}
	})
	return at.Sub(start)
}

func (r *quitRig) one(ws, q string, args ...any) string {
	r.t.Helper()
	// Loop 可能正在写库，只读连接也要等锁。
	db, err := sql.Open("sqlite", filepath.Join(r.root, "workspaces", ws, "runs.db")+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		r.t.Fatal(err)
	}
	defer db.Close()
	var out string
	if err := db.QueryRowContext(r.t.Context(), q, args...).Scan(&out); err != nil {
		r.t.Fatalf("%s：%v", q, err)
	}
	return out
}

func (r *quitRig) runState(ws, runID string) string {
	return r.one(ws, `SELECT state FROM runs WHERE id = ?`, runID)
}

func (r *quitRig) lastStep(ws, runID string) string {
	return r.one(ws, `SELECT COALESCE((SELECT status FROM run_steps WHERE run_id = ? ORDER BY ordinal DESC LIMIT 1), '')`, runID)
}

func (r *quitRig) audits(ws, action string) int {
	n, _ := strconv.Atoi(r.one(ws, `SELECT COUNT(*) FROM audit WHERE action = ?`, action))
	return n
}

func (r *quitRig) running() int {
	total := 0
	for _, ws := range []string{"星禾", "云杉"} {
		n, _ := strconv.Atoi(r.one(ws, `SELECT COUNT(*) FROM runs WHERE state = 'running'`))
		total += n
	}
	return total
}

func (r *quitRig) ticketState(ws, ticketID string) string {
	return r.one(ws, `SELECT state FROM approvals WHERE id = ?`, ticketID)
}

func (r *quitRig) assertExitedWithin(d time.Duration) {
	r.t.Helper()
	if d > r.grace {
		r.t.Fatalf("退出耗时 %v，超过有界等待 %v", d, r.grace)
	}
}

// gateProvider 在第 blockAt 次模型调用里阻塞到 ctx 结束，模拟流式中（模型调用中或步骤之间）。
// 之前的调用交给 inner。
type gateProvider struct {
	inner   llm.Provider
	blockAt int32
	calls   atomic.Int32
	entered chan struct{}
}

func (p *gateProvider) ID() string { return "gate" }

func (p *gateProvider) Chat(ctx context.Context, req llm.Request) (llm.Response, error) {
	if p.calls.Add(1) == p.blockAt {
		close(p.entered)
		<-ctx.Done()
		return llm.Response{}, ctx.Err()
	}
	return p.inner.Chat(ctx, req)
}

// heldProvider 的第一次模型调用等 release 关闭后才交给 inner。
type heldProvider struct {
	inner   llm.Provider
	release chan struct{}
	once    sync.Once
}

func (p *heldProvider) ID() string { return "held" }

func (p *heldProvider) Chat(ctx context.Context, req llm.Request) (llm.Response, error) {
	first := false
	p.once.Do(func() { first = true })
	if first {
		select {
		case <-p.release:
		case <-ctx.Done():
			return llm.Response{}, ctx.Err()
		}
	}
	return p.inner.Chat(ctx, req)
}

func exportThenSay(runID string) llm.Provider {
	return llm.NewMock(llm.MustNewScript(
		llm.CallTools(llm.ToolCall{ID: "call-" + runID, Name: "export_data", Arguments: `{"object":"客户","fields":["phone"]}`}),
		llm.Say("导出完成。"),
	))
}

// blockTool 是注册成 export_data 的计数工具。Execute 进入即计数，然后阻塞：
// 默认理会 ctx；ignoreCtx 为真时只等 release，模拟不理会取消的工具。
type blockTool struct {
	n         atomic.Int64
	entered   chan struct{}
	release   chan struct{}
	ignoreCtx bool
	once      sync.Once
}

func newBlockTool(ignoreCtx bool) *blockTool {
	return &blockTool{entered: make(chan struct{}), release: make(chan struct{}), ignoreCtx: ignoreCtx}
}

func (b *blockTool) Spec() tools.Spec { return tools.SpecExportData }

func (b *blockTool) N() int64 { return b.n.Load() }

func (b *blockTool) Execute(ctx context.Context, _ tools.Call) (tools.Result, error) {
	b.n.Add(1)
	b.once.Do(func() { close(b.entered) })
	if b.ignoreCtx {
		<-b.release
		return tools.Result{Content: "已执行", Summary: "已执行"}, nil
	}
	select {
	case <-ctx.Done():
		return tools.Result{}, ctx.Err()
	case <-b.release:
		return tools.Result{Content: "已执行", Summary: "已执行"}, nil
	}
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("等待超时：%s", what)
	}
}

// AC1：没有活跃工作（包括只有已暂缓的 awaiting_approval 运行）时，关窗口和退出都直接放行，不弹框。
func TestQuitWithoutActiveWorkPassesThrough(t *testing.T) {
	tool := session.NewExportCounter()
	r := newQuitRig(t, tool, nil)
	if !r.fire("close") || !r.fire("quit") {
		t.Fatal("空闲时退出被拦住")
	}
	r.tt.Frame()
	if r.tt.HasText(quitTitle) {
		t.Fatalf("空闲时弹了拦截框：%q", r.tt.Texts())
	}

	ask(t, r.tt, "导出客户")
	tk := r.waitDialog()
	r.tt.Key(0, mygoui.KeyEnter)
	r.pump("暂缓后空闲", func() bool { return r.s.state.Phase() == "等待确认" && r.idle() })
	if got := r.runState("星禾", tk.RunID); got != "awaiting_approval" {
		t.Fatalf("暂缓后运行=%s", got)
	}
	if !r.fire("close") || !r.fire("quit") {
		t.Fatal("只有已暂缓的运行时退出被拦住")
	}
	r.tt.Frame()
	if r.tt.HasText(quitTitle) || len(r.exited) != 0 || tool.N() != 0 {
		t.Fatalf("已暂缓时弹框或执行 texts=%q n=%d", r.tt.Texts(), tool.N())
	}
}

// AC2、AC8、AC9（确认框打开）：库里是 running 时选「仍退出」，运行落为 awaiting_approval，原票仍 pending 未消费。
// 重启后列表里是同一张票（ID 和哈希相同），批准后执行 1 次；再重启不会再执行。
func TestQuitWithDialogOpenKeepsSameTicket(t *testing.T) {
	for _, kind := range []string{"close", "quit"} {
		t.Run(kind, func(t *testing.T) {
			tool := session.NewExportCounter()
			r := newQuitRig(t, tool, nil)
			ask(t, r.tt, "导出客户")
			tk := r.waitDialog()
			if got := r.runState("星禾", tk.RunID); got != "running" {
				t.Fatalf("确认框打开时运行=%s", got)
			}
			r.interceptVia(kind)
			if !r.tt.HasText("1 张确认单在等待决定") || !r.tt.HasText("暂缓保留") || !r.tt.HasText("不会重跑") {
				t.Fatalf("拦截框没说明后果：%q", r.tt.Texts())
			}
			r.assertExitedWithin(r.leaveAndWait())
			if got := r.runState("星禾", tk.RunID); got != "awaiting_approval" {
				t.Fatalf("仍退出后运行=%s", got)
			}
			if got := r.ticketState("星禾", tk.ID); got != "pending" {
				t.Fatalf("仍退出后票据=%s", got)
			}
			if r.running() != 0 || tool.N() != 0 {
				t.Fatalf("退出后 running=%d n=%d", r.running(), tool.N())
			}

			r.start()
			list := shellList(r.s)
			if len(list) != 1 || list[0].ID != tk.ID || list[0].ParamsHash != tk.ParamsHash || list[0].RunID != tk.RunID {
				t.Fatalf("重启后列表=%+v want %s/%s", list, tk.ID, tk.ParamsHash)
			}
			r.click("重开 " + tk.ID)
			again := r.waitDialog()
			if again.ID != tk.ID || again.ParamsHash != tk.ParamsHash {
				t.Fatalf("重开的票据=%s/%s", again.ID, again.ParamsHash)
			}
			r.click("批准")
			r.pump("批准后执行", func() bool { return tool.N() == 1 && r.idle() && r.runState("星禾", tk.RunID) == "completed" })
			if got := r.ticketState("星禾", tk.ID); got != "consumed" {
				t.Fatalf("批准后票据=%s", got)
			}

			r.start()
			if list := shellList(r.s); len(list) != 0 || tool.N() != 1 {
				t.Fatalf("再次重启 list=%+v n=%d", list, tool.N())
			}
		})
	}
}

// AC2 后半：从待确认列表重开的确认框（Resume 堵在 Request 里）按同样方式验证。
func TestQuitWithReopenedDialogKeepsSameTicket(t *testing.T) {
	tool := session.NewExportCounter()
	r := newQuitRig(t, tool, nil)
	ask(t, r.tt, "导出客户")
	tk := r.waitDialog()
	r.tt.Key(0, mygoui.KeyEscape)
	r.pump("暂缓后空闲", func() bool { return r.s.state.Phase() == "等待确认" && r.idle() })

	r.start()
	r.click("重开 " + tk.ID)
	if again := r.waitDialog(); again.ID != tk.ID {
		t.Fatalf("重开的票据=%s", again.ID)
	}
	if !r.waiter.Busy(tk.RunID) {
		t.Fatal("重开后等待器里没有这趟运行")
	}
	r.interceptVia("quit")
	if !r.tt.HasText("1 张确认单在等待决定") {
		t.Fatalf("拦截框：%q", r.tt.Texts())
	}
	r.assertExitedWithin(r.leaveAndWait())
	if r.runState("星禾", tk.RunID) != "awaiting_approval" || r.ticketState("星禾", tk.ID) != "pending" || r.running() != 0 || tool.N() != 0 {
		t.Fatalf("仍退出后 run=%s ticket=%s running=%d n=%d", r.runState("星禾", tk.RunID), r.ticketState("星禾", tk.ID), r.running(), tool.N())
	}

	r.start()
	list := shellList(r.s)
	if len(list) != 1 || list[0].ID != tk.ID || list[0].ParamsHash != tk.ParamsHash {
		t.Fatalf("重启后列表=%+v", list)
	}
	r.click("重开 " + tk.ID)
	r.waitDialog()
	r.click("批准")
	r.pump("批准后执行", func() bool { return tool.N() == 1 && r.idle() && r.runState("星禾", tk.RunID) == "completed" })
	r.start()
	if list := shellList(r.s); len(list) != 0 || tool.N() != 1 {
		t.Fatalf("再次重启 list=%+v n=%d", list, tool.N())
	}
}

// AC3、AC9（流式中）：模型调用中或步骤之间选「仍退出」，运行落为 cancelled；重启后不续跑，工具计数不变。
func TestQuitWhileStreamingCancels(t *testing.T) {
	cases := []struct {
		name    string
		blockAt int32
		approve bool
		want    int64
	}{
		{name: "模型调用中", blockAt: 1},
		{name: "步骤之间", blockAt: 2, approve: true, want: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool := session.NewExportCounter()
			var gate atomic.Pointer[gateProvider]
			r := newQuitRig(t, tool, func(runID string) llm.Provider {
				p := &gateProvider{inner: exportThenSay(runID), blockAt: tc.blockAt, entered: make(chan struct{})}
				gate.Store(p)
				return p
			})
			ask(t, r.tt, "导出客户")
			if tc.approve {
				r.waitDialog()
				r.click("批准")
			}
			r.pump("模型调用阻塞", func() bool {
				p := gate.Load()
				if p == nil {
					return false
				}
				select {
				case <-p.entered:
					return r.s.state.StreamingRun != ""
				default:
					return false
				}
			})
			runID := r.s.state.StreamingRun
			if tool.N() != tc.want || r.runState("星禾", runID) != "running" {
				t.Fatalf("退出前 n=%d run=%s", tool.N(), r.runState("星禾", runID))
			}
			r.interceptVia("quit")
			if !r.tt.HasText("1 个回答正在进行") || r.tt.HasText("张确认单在等待决定") {
				t.Fatalf("拦截框：%q", r.tt.Texts())
			}
			r.assertExitedWithin(r.leaveAndWait())
			if got := r.runState("星禾", runID); got != "cancelled" {
				t.Fatalf("仍退出后运行=%s", got)
			}
			if r.running() != 0 {
				t.Fatalf("退出后仍有 running=%d", r.running())
			}

			r.start()
			if r.runState("星禾", runID) != "cancelled" || tool.N() != tc.want || len(shellList(r.s)) != 0 || r.audits("星禾", "executing_interrupted") != 0 {
				t.Fatalf("重启后 run=%s n=%d list=%+v", r.runState("星禾", runID), tool.N(), shellList(r.s))
			}
		})
	}
}

// AC9（工具执行中）：工具理会 ctx 时，「仍退出」中止它，步骤记 error，不重跑。
// Loop 里执行时运行记 cancelled；从列表重开后执行时，Resume 把运行记 failed。
func TestQuitWhileToolExecutesAbortsWithoutRerun(t *testing.T) {
	t.Run("循环内批准", func(t *testing.T) {
		tool := newBlockTool(false)
		r := newQuitRig(t, tool, nil)
		ask(t, r.tt, "导出客户")
		tk := r.waitDialog()
		r.click("批准")
		waitClosed(t, tool.entered, "工具开始执行")
		r.tt.Frame()
		if r.lastStep("星禾", tk.RunID) != "executing" || r.runState("星禾", tk.RunID) != "running" {
			t.Fatalf("执行中 step=%s run=%s", r.lastStep("星禾", tk.RunID), r.runState("星禾", tk.RunID))
		}
		r.interceptVia("close")
		if !r.tt.HasText("1 个回答正在进行") {
			t.Fatalf("拦截框：%q", r.tt.Texts())
		}
		r.assertExitedWithin(r.leaveAndWait())
		if r.runState("星禾", tk.RunID) != "cancelled" || r.lastStep("星禾", tk.RunID) != "error" || r.running() != 0 {
			t.Fatalf("仍退出后 run=%s step=%s running=%d", r.runState("星禾", tk.RunID), r.lastStep("星禾", tk.RunID), r.running())
		}
		r.start()
		if r.runState("星禾", tk.RunID) != "cancelled" || tool.N() != 1 || len(shellList(r.s)) != 0 || r.audits("星禾", "executing_interrupted") != 0 {
			t.Fatalf("重启后 run=%s n=%d list=%+v", r.runState("星禾", tk.RunID), tool.N(), shellList(r.s))
		}
	})
	t.Run("重开后批准", func(t *testing.T) {
		tool := newBlockTool(false)
		r := newQuitRig(t, tool, nil)
		ask(t, r.tt, "导出客户")
		tk := r.waitDialog()
		r.tt.Key(0, mygoui.KeyEscape)
		r.pump("暂缓后空闲", func() bool { return r.s.state.Phase() == "等待确认" && r.idle() })
		r.start()
		r.click("重开 " + tk.ID)
		r.waitDialog()
		r.click("批准")
		waitClosed(t, tool.entered, "工具开始执行")
		r.interceptVia("quit")
		if !r.tt.HasText("1 个确认单正在恢复执行") {
			t.Fatalf("拦截框：%q", r.tt.Texts())
		}
		r.assertExitedWithin(r.leaveAndWait())
		if r.runState("星禾", tk.RunID) != "failed" || r.lastStep("星禾", tk.RunID) != "error" || r.running() != 0 {
			t.Fatalf("仍退出后 run=%s step=%s running=%d", r.runState("星禾", tk.RunID), r.lastStep("星禾", tk.RunID), r.running())
		}
		r.start()
		if r.runState("星禾", tk.RunID) != "failed" || tool.N() != 1 || len(shellList(r.s)) != 0 {
			t.Fatalf("重启后 run=%s n=%d list=%+v", r.runState("星禾", tk.RunID), tool.N(), shellList(r.s))
		}
	})
}

// AC4：选「取消退出」（Enter 默认项）或按 Escape，应用继续，确认框仍可决定，最终只执行一次。
// 拦截框上的 Enter 不会批准、拒绝或暂缓下面的确认单。
func TestQuitCancelKeepsDialogDecidable(t *testing.T) {
	tool := session.NewExportCounter()
	r := newQuitRig(t, tool, nil)
	ask(t, r.tt, "导出客户")
	tk := r.waitDialog()
	stillWaiting := func() {
		t.Helper()
		a := r.waiter.Activity()
		if r.s.quitAsked() || r.s.leaving() || len(a.Waiting) != 1 || a.Waiting[0].ID != tk.ID {
			t.Fatalf("取消后 asked=%v leaving=%v waiting=%+v", r.s.quitAsked(), r.s.leaving(), a.Waiting)
		}
		if r.ticketState("星禾", tk.ID) != "pending" || r.runState("星禾", tk.RunID) != "running" || tool.N() != 0 || len(r.exited) != 0 {
			t.Fatalf("取消后 ticket=%s run=%s n=%d", r.ticketState("星禾", tk.ID), r.runState("星禾", tk.RunID), tool.N())
		}
		r.pump("确认框回来", func() bool { return r.tt.HasText("批准") && !r.tt.HasText(quitTitle) })
	}

	r.interceptVia("quit")
	r.tt.Key(0, mygoui.KeyEnter)
	r.tt.Frame()
	stillWaiting()

	r.interceptVia("close")
	r.tt.Key(0, mygoui.KeyEscape)
	r.tt.Frame()
	stillWaiting()

	r.interceptVia("close")
	r.click("取消退出")
	r.tt.Frame()
	stillWaiting()

	// 拦截框关掉后确认框重新出现，Enter 仍是它的默认项「暂缓」，不会因为刚才的拦截框变成批准。
	r.tt.Key(0, mygoui.KeyEnter)
	r.pump("Enter 暂缓", func() bool { return r.idle() && r.s.state.Phase() == "等待确认" })
	if tool.N() != 0 || r.runState("星禾", tk.RunID) != "awaiting_approval" || r.ticketState("星禾", tk.ID) != "pending" {
		t.Fatalf("确认框上的 Enter n=%d run=%s ticket=%s", tool.N(), r.runState("星禾", tk.RunID), r.ticketState("星禾", tk.ID))
	}
	r.click("重开 " + tk.ID)
	r.waitDialog()
	r.click("批准")
	r.pump("批准后完成", func() bool { return tool.N() == 1 && r.idle() && r.runState("星禾", tk.RunID) == "completed" })
	if !r.fire("close") {
		t.Fatal("完成后退出仍被拦住")
	}
	if tool.N() != 1 {
		t.Fatalf("执行 %d 次", tool.N())
	}
}

// AC5：同一次退出里 OnClose 和 OnBeforeQuit 只弹一个拦截框；框开着时再关、再 Quit 不叠第二个。
// 确认单不在当前工作区时同样拦截。
func TestQuitDialogOpensOnceAcrossWorkspaces(t *testing.T) {
	tool := session.NewExportCounter()
	// 确认框是模态的，打开后点不到侧边栏。所以先在星禾提问，模型还在调用时切到云杉，再放行模型生成确认单。
	release := make(chan struct{})
	r := newQuitRig(t, tool, func(runID string) llm.Provider {
		return &heldProvider{inner: exportThenSay(runID), release: release}
	})
	ask(t, r.tt, "导出客户")
	r.pump("流式中", func() bool { return r.s.state.StreamingRun != "" })
	r.click("云杉")
	close(release)
	var tk approval.Ticket
	r.pump("确认单在星禾等待", func() bool {
		a := r.waiter.Activity()
		if len(a.Waiting) == 1 {
			tk = a.Waiting[0]
		}
		return tk.ID != "" && r.s.state.ActiveID() == "云杉" && !r.tt.HasText("批准")
	})
	if tk.WorkspaceID != "星禾" {
		t.Fatalf("票据工作区=%s", tk.WorkspaceID)
	}

	if r.fire("close") || r.fire("quit") || r.fire("close") || r.fire("quit") {
		t.Fatal("确认单在别的工作区时退出没被拦住")
	}
	r.pump("拦截框", func() bool { return r.tt.HasText(quitTitle) })
	// AlertDialog 把标题同时作为面板标签和正文，标题会出现两次；按钮只有一个。
	n := 0
	for _, text := range r.tt.Texts() {
		if text == "取消退出" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("拦截框出现 %d 次：%q", n, r.tt.Texts())
	}
	if !r.tt.HasText("其中 1 张不在当前工作区") {
		t.Fatalf("没有说明其他工作区的确认单：%q", r.tt.Texts())
	}
	r.click("取消退出")
	r.click("星禾")
	if again := r.waitDialog(); again.ID != tk.ID {
		t.Fatalf("切回后的票据=%s", again.ID)
	}
	r.click("暂缓")
	r.pump("暂缓后空闲", func() bool { return r.idle() })
	if tool.N() != 0 || r.ticketState("星禾", tk.ID) != "pending" {
		t.Fatalf("n=%d ticket=%s", tool.N(), r.ticketState("星禾", tk.ID))
	}
}

// AC7：启动扫描每次进程启动只跑一次。已暂缓的运行状态不变，每次启动追加一条 approval_restored；
// 列表刷新、切换工作区、重开票据、查看轨迹都不追加。
func TestStartupScanOncePerStart(t *testing.T) {
	tool := session.NewExportCounter()
	r := newQuitRig(t, tool, nil)
	ask(t, r.tt, "导出客户")
	tk := r.waitDialog()
	r.tt.Key(0, mygoui.KeyEscape)
	r.pump("暂缓后空闲", func() bool { return r.s.state.Phase() == "等待确认" && r.idle() })
	if r.audits("星禾", "approval_restored") != 0 {
		t.Fatal("第一次启动时还没有挂起的票，却写了恢复审计")
	}

	r.start()
	if r.audits("星禾", "approval_restored") != 1 || r.runState("星禾", tk.RunID) != "awaiting_approval" {
		t.Fatalf("第二次启动 audits=%d run=%s", r.audits("星禾", "approval_restored"), r.runState("星禾", tk.RunID))
	}
	r.s.reloadList()
	r.s.reloadList()
	r.click("云杉")
	r.click("星禾")
	r.click("运行轨迹")
	r.tt.Frame()
	r.s.trace.open = false
	r.tt.Frame()
	r.click("重开 " + tk.ID)
	r.waitDialog()
	r.click("暂缓")
	r.pump("重开后暂缓", func() bool { return r.idle() })
	r.s.reloadList()
	if r.audits("星禾", "approval_restored") != 1 || r.audits("云杉", "approval_restored") != 0 {
		t.Fatalf("刷新、切换、重开或查看轨迹追加了恢复审计 星禾=%d 云杉=%d", r.audits("星禾", "approval_restored"), r.audits("云杉", "approval_restored"))
	}

	r.start()
	if r.audits("星禾", "approval_restored") != 2 || r.runState("星禾", tk.RunID) != "awaiting_approval" || tool.N() != 0 {
		t.Fatalf("第三次启动 audits=%d run=%s n=%d", r.audits("星禾", "approval_restored"), r.runState("星禾", tk.RunID), tool.N())
	}
}

// 子进程测试：AC6 硬杀和 AC8 超时退出都要真的结束进程，进程里卡住的 goroutine 才不会留到「重启」之后。

const (
	helperEnv     = "LANTERN_QUIT_HELPER"
	helperRootEnv = "LANTERN_QUIT_ROOT"
	helperMarkEnv = "LANTERN_QUIT_MARK"
)

// markTool 每次 Execute 进入时在 mark 文件追加一行，然后一直阻塞，不理会 ctx。
type markTool struct{ path string }

func (m markTool) Spec() tools.Spec { return tools.SpecExportData }

func (m markTool) Execute(context.Context, tools.Call) (tools.Result, error) {
	f, err := os.OpenFile(m.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err == nil {
		_, _ = f.WriteString("execute\n")
		_ = f.Close()
	}
	select {}
}

// TestQuitHelperProcess 只在子进程里做事；直接运行时立即返回。
func TestQuitHelperProcess(t *testing.T) {
	mode := os.Getenv(helperEnv)
	if mode == "" {
		return
	}
	root, mark := os.Getenv(helperRootEnv), os.Getenv(helperMarkEnv)
	waiter := session.NewWaiter()
	runner := &session.LoopRunner{Root: root, Waiter: waiter, Tool: markTool{path: mark}, Demo: true}
	s, err := newShell(context.Background(), Options{Root: root, Workspaces: []session.Workspace{{ID: "星禾"}, {ID: "云杉"}}, Runner: runner})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	s.post = func(fn func()) { s.hops <- fn }
	s.grace = time.Second
	s.exit = func() {
		appendLine(mark, "exit "+strconv.FormatInt(time.Now().UnixNano(), 10))
		os.Exit(0)
	}
	tt := tester(s)
	tt.Frame()
	ask(t, tt, "导出客户")
	// kill：循环内批准，工具执行中被父进程 SIGKILL。
	// grace：先暂缓，再从列表重开后批准。Resume 同步调用工具、没有放弃时限，不理会 ctx 的工具才会拖过有界等待。
	var ticketID string
	approved, deferred, reopened, left := false, false, false, false
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		tt.Frame()
		s.mu.Lock()
		if s.dialog != nil {
			ticketID = s.dialog.ID
		}
		s.mu.Unlock()
		switch {
		case mode == "grace" && !deferred && tt.HasText("批准"):
			tt.Key(0, mygoui.KeyEscape)
			deferred = true
		case mode == "grace" && deferred && !reopened && s.activity().empty() && tt.HasText("重开 "+ticketID):
			_ = tt.Click("重开 " + ticketID)
			reopened = true
		case !approved && (mode == "kill" || reopened) && tt.HasText("批准"):
			_ = tt.Click("批准")
			approved = true
		}
		if mode == "grace" && !left && countLines(mark, "execute") == 1 {
			e := &mygo.QuitEvent{}
			s.onBeforeQuit(e)
			tt.Frame()
			if !e.DefaultPrevented() || tt.Click("仍退出") != nil {
				os.Exit(4)
			}
			appendLine(mark, "leave "+strconv.FormatInt(time.Now().UnixNano(), 10))
			left = true
		}
		time.Sleep(5 * time.Millisecond)
	}
	os.Exit(3)
}

func appendLine(path, line string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = f.WriteString(line + "\n")
	_ = f.Close()
}

func countLines(path, prefix string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), prefix) {
			n++
		}
	}
	return n
}

func stampOf(t *testing.T, path, prefix string) time.Time {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, prefix+" "); ok {
			ns, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			return time.Unix(0, ns)
		}
	}
	t.Fatalf("%s 里没有 %s", path, prefix)
	return time.Time{}
}

// startHelper 起子进程，等它的工具进入 Execute。
func startHelper(t *testing.T, mode string) (*exec.Cmd, string, string) {
	t.Helper()
	root := t.TempDir()
	createWorkspaces(t, root, "星禾", "云杉")
	mark := filepath.Join(t.TempDir(), "mark")
	cmd := exec.Command(os.Args[0], "-test.run=^TestQuitHelperProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), helperEnv+"="+mode, helperRootEnv+"="+root, helperMarkEnv+"="+mark)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	deadline := time.Now().Add(30 * time.Second)
	for countLines(mark, "execute") == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("子进程的工具没有开始执行：%s", stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cmd, root, mark
}

func onlyRun(t *testing.T, root string) string {
	t.Helper()
	db := openRunsRO(t, root, "星禾")
	defer db.Close()
	var id string
	if err := db.QueryRowContext(t.Context(), `SELECT id FROM runs`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// assertRecoveredOnStart 是「重启」：新 shell 的启动扫描把停在 executing 的运行标 failed、写 executing_interrupted，不重放。
// 不新提问，用 -trace 的只读输出就能看到这次失败迁移；再重启不会重复处理。
func assertRecoveredOnStart(t *testing.T, root, mark, runID string) {
	t.Helper()
	r := &quitRig{t: t, root: root, tool: session.NewExportCounter(), grace: 2 * time.Second}
	if got := r.runState("星禾", runID); got != "running" || r.lastStep("星禾", runID) != "executing" {
		t.Fatalf("进程结束后、重启前 run=%s step=%s", got, r.lastStep("星禾", runID))
	}
	r.start()
	if r.runState("星禾", runID) != "failed" || r.audits("星禾", "executing_interrupted") != 1 {
		t.Fatalf("启动扫描后 run=%s interrupted=%d", r.runState("星禾", runID), r.audits("星禾", "executing_interrupted"))
	}
	if !strings.Contains(shellNotice(r.s), "1 个中断的运行已标为失败") {
		t.Fatalf("notice=%q", shellNotice(r.s))
	}
	var out bytes.Buffer
	if err := WriteTrace(t.Context(), &out, root, "星禾", runID); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"状态 失败", "→ 失败", "执行中断，重启后不会重跑。"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("-trace 缺少 %q：\n%s", want, out.String())
		}
	}
	r.start()
	if countLines(mark, "execute") != 1 || r.audits("星禾", "executing_interrupted") != 1 || len(shellList(r.s)) != 0 || r.tool.(*session.CountTool).N() != 0 {
		t.Fatalf("再次重启 execute=%d interrupted=%d list=%+v", countLines(mark, "execute"), r.audits("星禾", "executing_interrupted"), shellList(r.s))
	}
}

// AC6：工具执行中被 SIGKILL 后重启，启动扫描标 failed 并写 executing_interrupted，计数不变。
func TestHardKillDuringToolRecoversOnStart(t *testing.T) {
	cmd, root, mark := startHelper(t, "kill")
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	assertRecoveredOnStart(t, root, mark, onlyRun(t, root))
}

// AC8：工具不理会 ctx 时，「仍退出」在有界等待到点后仍然退出进程；下次启动按 AC6 兜底。
func TestQuitGraceTimeoutStillExits(t *testing.T) {
	cmd, root, mark := startHelper(t, "grace")
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("子进程退出：%v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("有界等待后进程没有退出")
	}
	waited := stampOf(t, mark, "exit").Sub(stampOf(t, mark, "leave"))
	if waited < 900*time.Millisecond || waited >= 3*time.Second {
		t.Fatalf("仍退出到退出耗时 %v，应等满 1s 时限，且明显短于默认的 %v", waited, quitGrace)
	}
	assertRecoveredOnStart(t, root, mark, onlyRun(t, root))
}

func TestStartupNoticeSeparatesInterruptedFromResidual(t *testing.T) {
	for _, c := range []struct {
		interrupted, illegal int
		want                 string
	}{
		{0, 0, ""},
		{1, 0, "启动扫描：1 个中断的运行已标为失败，不会重跑。"},
		{0, 2, "启动扫描：2 个无法恢复的残留运行已标为失败，不会重跑。"},
		{3, 1, "启动扫描：3 个中断的运行、1 个无法恢复的残留运行已标为失败，不会重跑。"},
	} {
		if got := startupNotice(c.interrupted, c.illegal); got != c.want {
			t.Errorf("startupNotice(%d, %d)=%q，想要 %q", c.interrupted, c.illegal, got, c.want)
		}
	}
}

func TestQuitGraceDefaultIsFiveSeconds(t *testing.T) {
	if quitGrace != 5*time.Second {
		t.Fatalf("quitGrace=%v", quitGrace)
	}
}
