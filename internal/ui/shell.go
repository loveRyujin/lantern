package ui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/egoist/mygo"
	mygoui "github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown"
	"github.com/loveRyujin/lantern/core/approval"
	"github.com/loveRyujin/lantern/core/events"
	"github.com/loveRyujin/lantern/core/pyrt"
	"github.com/loveRyujin/lantern/core/run"
	"github.com/loveRyujin/lantern/core/tools"
	"github.com/loveRyujin/lantern/core/workspace"
	"github.com/loveRyujin/lantern/internal/ui/session"
)

// 没有工作区时创建这两个，方便本机直接开窗演示。已有工作区则保持不动。
var demoWorkspaceIDs = []string{"星禾", "云杉"}

// Options 控制产品窗口。Workspaces 非空时不再读磁盘。
type Options struct {
	// Root 是应用数据目录。空则用 workspace.DefaultRoot。
	Root string
	// Workspaces 直接指定侧边栏。测试用它，避免碰本机数据目录。
	Workspaces []session.Workspace
	// Runner 把提问写成 core/events。空则用确定性演示循环，经真实 Agent 循环确认，不联网。
	Runner session.Runner
	// CheckPrompt 非空时，窗口起来后自动提这一问，回答结束就退出。
	// 给本机冒烟用。默认测试不设置它，CI 也不会打开窗口。
	CheckPrompt string
	// CheckShot 非空时，自动提问成功后把窗口写成这张 PNG。
	CheckShot string
	// Python 是可信方已显式准备的运行时。非空且 Runner 为空时，窗口用场景 1 出图演示；
	// 零值表示没有准备，只出 run_sql 的结构化图表，run_python 报环境不可用。
	Python *pyrt.Runtime
	// DemoToolDelay 大于零时，演示循环的 export_data 在 Execute 里先等这么久（理会 ctx）再计数执行。
	// 给真实窗口里「工具执行中」退出和硬杀的验证用，默认零。
	DemoToolDelay time.Duration
}

// shell 是主窗口。mygo 类型只留在这个文件所在的包里。
type shell struct {
	win    *mygo.Window
	state  *session.State
	runner session.Runner
	ctx    context.Context
	root   string
	shot   string
	md     markdown.Options

	waiter *session.Waiter
	mu     sync.Mutex
	dialog *approval.Ticket
	asking bool
	list   []approval.Ticket
	notice string
	// hops 把恢复结果送回界面线程。没有窗口时由 view 排空，避免后台写会话状态。
	hops  chan func()
	trace tracePanel

	analysis     *session.Analysis
	ownsAnalysis bool
	chartViews   map[string]*chartView

	// runCtx 只给提问和 Resume 用，「仍退出」时取消。读列表和轨迹仍用 ctx。
	runCtx   context.Context
	stopRuns context.CancelFunc
	// post 把函数排到界面线程。有窗口时用 Window.Update；无窗口测试可以接到 hops 上，让提问在后台跑。
	post func(func())
	// asks 和 resumes 是本进程里还没收尾的提问和重开，由 mu 保护。work 计同一批后台 goroutine。
	asks     map[string]struct{}
	resumes  map[string]struct{}
	work     sync.WaitGroup
	quit     quitPhase
	quitWork quitWork
	// grace 为零时用 quitGrace。exit 在有界等待之后调用，产品窗口里是 App.Quit。
	grace time.Duration
	exit  func()
	// overlay 是上一帧画的弹窗，只在界面线程读写。
	overlay overlayKind
}

type overlayKind int

const (
	overlayNone overlayKind = iota
	overlayConfirm
	overlayQuit
)

// Run 打开主窗口并进入事件循环。
// 侧边栏切换工作区，对话区用 markdown.Stream 显示 token。
// 事件从 core/events.Stream 进来，在 Window.Update 里改状态。
func Run(opts Options) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := newShell(ctx, opts)
	if err != nil {
		return err
	}
	if s.ownsAnalysis {
		defer s.analysis.Close()
	}
	mygo.Theme.SetSource(mygo.ThemeLight)
	s.exit = mygo.App.Quit
	errc := make(chan error, 1)
	mygo.App.WhenReady(func() {
		// 关窗口先到 OnClose，再进 Quit 流程到 OnBeforeQuit；Cmd+Q、App.Quit 和第一个 SIGINT/SIGTERM 只到后者。
		// 第二个信号由 mygo 直接 os.Exit，不经过这里，留给启动扫描。
		mygo.App.OnBeforeQuit(s.onBeforeQuit)
		s.win = mygo.NewWindow(mygo.WindowOptions{
			Title:   "lantern",
			Width:   1080,
			Height:  720,
			Content: mygoui.View(s.view),
		})
		s.win.OnClose(s.onClose)
		if opts.CheckPrompt == "" {
			return
		}
		go s.runCheck(errc, func() error {
			if s.analysis != nil {
				return s.autoChart(opts.CheckPrompt)
			}
			return s.autoAsk(opts.CheckPrompt)
		})
	})
	if err := mygo.App.Run(); err != nil {
		return err
	}
	if opts.CheckPrompt == "" {
		return nil
	}
	select {
	case err := <-errc:
		return err
	default:
		return errors.New("ui: 自动提问没有返回结果")
	}
}

func newShell(ctx context.Context, opts Options) (*shell, error) {
	if ctx == nil {
		return nil, errors.New("ui: ctx 不能为 nil")
	}
	root := opts.Root
	workspaces := opts.Workspaces
	if len(workspaces) == 0 {
		if strings.TrimSpace(root) == "" {
			var err error
			root, err = workspace.DefaultRoot()
			if err != nil {
				return nil, err
			}
		}
		var err error
		workspaces, err = loadWorkspaces(ctx, root)
		if err != nil {
			return nil, err
		}
	}
	st, err := session.New(workspaces)
	if err != nil {
		return nil, err
	}
	runner := opts.Runner
	var waiter *session.Waiter
	owns := false
	if runner == nil {
		if strings.TrimSpace(root) == "" {
			return nil, errors.New("ui: 演示循环需要数据目录")
		}
		waiter = session.NewWaiter()
		if opts.Python != nil {
			runner = session.NewChartDemoRunner(root, waiter, &session.Analysis{Runtime: *opts.Python})
			owns = true
		} else {
			lr := session.NewDemoRunner(root, waiter)
			if opts.DemoToolDelay > 0 {
				lr.Tool = delayTool{Tool: lr.Tool, delay: opts.DemoToolDelay}
			}
			runner = lr
		}
	}
	s := &shell{
		state: st, runner: runner, ctx: ctx, root: root, shot: opts.CheckShot, waiter: waiter,
		hops: make(chan func(), 8),
		asks: map[string]struct{}{}, resumes: map[string]struct{}{},
	}
	s.runCtx, s.stopRuns = context.WithCancel(ctx)
	// AtOnce 让每个 token 在下一帧就出现。负的 Fade 关掉淡入，测试帧才稳定。
	s.md.Reveal = &markdown.Reveal{Unit: markdown.AtOnce, Fade: -1}
	s.md.WrapCode = true
	if lr, ok := runner.(*session.LoopRunner); ok && lr.Waiter != nil {
		s.waiter = lr.Waiter
		lr.Waiter.SetOnShow(s.showTicket)
	}
	if lr, ok := runner.(*session.LoopRunner); ok && lr.Analysis != nil {
		s.analysis, s.ownsAnalysis = lr.Analysis, owns
	}
	if err := s.recoverAtStart(); err != nil {
		s.stopRuns()
		return nil, err
	}
	s.reloadList()
	return s, nil
}

func (s *shell) stop() {
	if s.stopRuns != nil {
		s.stopRuns()
	}
}

// recoverAtStart 是桌面启动扫描（FR-SM-5）：第一次读待确认列表前，把每个已有工作区补到当前 schema，
// 再跑一次 Recover。停在 executing 的运行标 failed 并写 executing_interrupted，仍挂起的确认单追加
// approval_restored。Run 只建一个 shell，所以每次进程启动只扫一次；之后的列表刷新、切换工作区、
// 重开票据和轨迹查看仍然只读，不追加恢复审计。
// 这时本进程还没有任何活跃运行，不需要跳过谁。
func (s *shell) recoverAtStart() error {
	if strings.TrimSpace(s.root) == "" || s.state == nil {
		return nil
	}
	interrupted, illegal := 0, 0
	for _, ws := range s.state.Workspaces {
		h, err := workspace.OpenPrepared(s.ctx, s.root, ws.ID)
		if err != nil {
			return err
		}
		rep, err := run.New(h.Runs).Recover(s.ctx)
		cerr := h.Close()
		if err != nil {
			return fmt.Errorf("ui: 启动扫描 %s: %w", ws.ID, err)
		}
		if cerr != nil {
			return cerr
		}
		interrupted += len(rep.Interrupted)
		illegal += len(rep.Illegal)
	}
	s.notice = startupNotice(interrupted, illegal)
	return nil
}

// startupNotice 把执行中断和无法恢复的残留分开说：前者是工具跑到一半，后者是没有可恢复的断点或确认单。
func startupNotice(interrupted, illegal int) string {
	var parts []string
	if interrupted > 0 {
		parts = append(parts, fmt.Sprintf("%d 个中断的运行", interrupted))
	}
	if illegal > 0 {
		parts = append(parts, fmt.Sprintf("%d 个无法恢复的残留运行", illegal))
	}
	if len(parts) == 0 {
		return ""
	}
	return "启动扫描：" + strings.Join(parts, "、") + "已标为失败，不会重跑。"
}

func loadWorkspaces(ctx context.Context, root string) ([]session.Workspace, error) {
	entries, err := workspace.List(root)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		for _, id := range demoWorkspaceIDs {
			if err := ensureWorkspace(ctx, root, id); err != nil {
				return nil, err
			}
		}
		entries, err = workspace.List(root)
		if err != nil {
			return nil, err
		}
	}
	if len(entries) == 0 {
		return nil, errors.New("ui: 没有工作区")
	}
	out := make([]session.Workspace, len(entries))
	for i, ent := range entries {
		out[i] = session.Workspace{ID: ent.ID}
	}
	return out, nil
}

func ensureWorkspace(ctx context.Context, root, id string) error {
	h, err := workspace.Create(ctx, root, id)
	if err != nil && errors.Is(err, workspace.ErrExists) {
		h, err = workspace.Open(ctx, root, id)
	}
	if err != nil {
		return err
	}
	return h.Close()
}

func (s *shell) view(c *mygoui.Context) {
	s.drainHops()
	t := c.Theme()
	mygoui.Column(c).Fill().Children(func() {
		mygoui.Row(c).Padding(12, 16).Gap(12).AlignItems(mygoui.Center).
			BorderWidth(0, 0, 1, 0).BorderColor(t.Border).Children(func() {
			mygoui.Text(c, "lantern").Bold()
			mygoui.Text(c, "mygo v"+ToolkitVersion()).TextColor(t.TextMuted)
			mygoui.Spacer(c)
			mygoui.Text(c, s.phaseLabel()).TextColor(t.TextMuted)
		})
		mygoui.Row(c).Grow(1).Children(func() {
			s.sidebar(c)
			if s.trace.open {
				s.traceView(c)
			} else {
				s.conversation(c)
			}
		})
	})
	s.dialogs(c)
}

// dialogs 每帧最多画一个弹窗：拦截框打开时不画确认框。
// mygo 按位置给弹层节点编号，AutoFocus 只在节点诞生那一帧生效。两种弹窗直接互换会复用节点，
// Enter 默认项就可能落到「批准」上。所以换弹窗时空出一帧不画弹层，让新弹窗的节点重新诞生。
func (s *shell) dialogs(c *mygoui.Context) {
	want := overlayNone
	switch {
	case s.quitAsked():
		want = overlayQuit
	case !s.leaving() && s.dialogOpen():
		want = overlayConfirm
	}
	if want != overlayNone && s.overlay != overlayNone && want != s.overlay {
		s.overlay = overlayNone
		if s.win != nil {
			s.win.Invalidate()
		}
		return
	}
	s.overlay = want
	switch want {
	case overlayQuit:
		s.quitDialog(c)
	case overlayConfirm:
		s.confirm(c)
	}
}

func (s *shell) sidebar(c *mygoui.Context) {
	t := c.Theme()
	mygoui.Column(c).Width(220).HeightPercent(100).Padding(12, 12).Gap(8).
		BorderWidth(0, 1, 0, 0).BorderColor(t.Border).Children(func() {
		mygoui.Text(c, "工作区").Bold()
		for i, ws := range s.state.Workspaces {
			b := mygoui.Button(c, ws.ID)
			if i == s.state.Active {
				b = b.Background(t.Accent)
			}
			if b.Clicked() {
				s.state.Switch(ws.ID)
				s.dropCharts()
				s.mu.Lock()
				s.notice = ""
				s.mu.Unlock()
				s.reloadList()
			}
		}
		mygoui.Text(c, "会话").TextColor(t.TextMuted)
		mygoui.Text(c, "当前会话")
		if text := s.banner(); text != "" {
			mygoui.Text(c, text).TextColor(t.TextMuted)
		}
		mygoui.Text(c, "待确认").Bold()
		s.pendingList(c)
		if mygoui.Button(c, "运行轨迹").Clicked() {
			s.openTrace("")
		}
	})
}

func (s *shell) pendingList(c *mygoui.Context) {
	t := c.Theme()
	s.mu.Lock()
	list := append([]approval.Ticket(nil), s.list...)
	notice := s.notice
	s.mu.Unlock()
	if len(list) == 0 {
		label := "没有待确认"
		if s.dialogOpen() {
			label = "正在等待这张确认单的决定"
		}
		mygoui.Text(c, label).TextColor(t.TextMuted)
	}
	for _, tk := range list {
		label := "重开 " + tk.ID
		if mygoui.Button(c, label).Clicked() {
			s.reopen(tk)
		}
	}
	if notice != "" {
		mygoui.Text(c, notice).TextColor(t.TextMuted)
	}
}

func (s *shell) conversation(c *mygoui.Context) {
	t := c.Theme()
	th := s.state.Thread(s.state.ActiveID())
	// 行里的列默认按内容高度排，回答和图表一多就把提问框挤出窗口。占满行高，滚动区才会收住。
	mygoui.Column(c).Grow(1).HeightPercent(100).Children(func() {
		mygoui.Scroll(c).Grow(1).Padding(12, 16).Children(func() {
			if len(th.Messages) == 0 {
				mygoui.Text(c, "在这个工作区里提问。回答会按 token 出现。").TextColor(t.TextMuted)
				return
			}
			mygoui.Column(c).Gap(12).Children(func() {
				for i := range th.Messages {
					s.bubble(c, th.Messages[i])
				}
			})
		})
		mygoui.Row(c).Padding(12, 16).Gap(8).AlignItems(mygoui.Center).
			BorderWidth(1, 0, 0, 0).BorderColor(t.Border).Children(func() {
			in := mygoui.TextInput(c, &s.state.Draft).Placeholder("在这里提问").Label("提问框").Grow(1)
			busy := s.state.StreamingRun != "" || s.leaving()
			if busy {
				mygoui.Button(c, "发送").Disabled(true)
				return
			}
			empty := strings.TrimSpace(s.state.Draft) == ""
			if (mygoui.PrimaryButton(c, "发送").Disabled(empty).Clicked() || in.Submitted()) && !empty {
				s.submit()
			}
		})
	})
}

func (s *shell) bubble(c *mygoui.Context, m session.Message) {
	t := c.Theme()
	if m.Role == session.RoleUser {
		mygoui.Row(c).Children(func() {
			mygoui.Spacer(c)
			mygoui.Text(c, m.Text).Padding(8, 12).Radius(12).Background(t.Surface)
		})
		return
	}
	mygoui.Column(c).Gap(6).Children(func() {
		for _, step := range m.Steps {
			mygoui.Text(c, step.Line()).TextColor(t.TextMuted)
		}
		if m.Approval != "" {
			mygoui.Text(c, m.Approval).TextColor(t.TextMuted)
		}
		s.detail(c, m.RunID)
		switch {
		case m.Text == "" && !m.Done:
			mygoui.Text(c, "等待回复…").TextColor(t.TextMuted)
		case m.Text == "" && m.Done:
			mygoui.Text(c, emptyDoneLabel(m.Status)).TextColor(t.TextMuted)
		case m.Done:
			markdown.View(c, m.Text, &s.md)
		default:
			markdown.Stream(c, m.Text, &s.md)
		}
		s.charts(c, m)
		if m.Note != "" {
			mygoui.Text(c, m.Note).TextColor(t.TextMuted)
		}
		if m.Done && m.RunID != "" && mygoui.Button(c, "查看轨迹").Clicked() {
			s.openTrace(m.RunID)
		}
	})
}

func emptyDoneLabel(status string) string {
	switch status {
	case events.DoneFailed:
		return "这次运行失败。"
	case session.StatusAwaiting:
		return "等待确认。运行仍挂起，可从列表重开。"
	default:
		return "没有回复。"
	}
}

func (s *shell) phaseLabel() string {
	if s.dialogOpen() {
		return "等待确认"
	}
	return s.state.Phase()
}

func (s *shell) dialogOpen() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asking && s.dialog != nil && s.state != nil && s.dialog.WorkspaceID == s.state.ActiveID()
}

func (s *shell) banner() string {
	if lr, ok := s.runner.(*session.LoopRunner); ok && lr.Demo && lr.Analysis != nil {
		return "演示：确定性模型按场景 1 调用本运行真实的 run_sql / run_python。柱状图来自 run_sql 结果，PNG 从分析会话取回，不联网。"
	}
	if lr, ok := s.runner.(*session.LoopRunner); ok && lr.Demo {
		return "演示：确定性模型，经真实 Agent 循环。确认单来自 Core，不联网。"
	}
	if _, ok := s.runner.(session.ScriptedRunner); ok {
		return "本地演示回答，不联网。"
	}
	return ""
}

func (s *shell) detail(c *mygoui.Context, runID string) {
	tk, ok := s.dialogFor(runID)
	if !ok {
		return
	}
	t := c.Theme()
	mygoui.Text(c, "工具 "+tk.Action)
	mygoui.Text(c, session.RiskLine(tk.Risk))
	mygoui.Text(c, "哈希 "+tk.ParamsHash)
	mygoui.Text(c, "参数 "+string(tk.Params))
	mygoui.Text(c, "参数按纯文本展示，不执行。").TextColor(t.TextMuted)
}

func (s *shell) dialogFor(runID string) (approval.Ticket, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.asking || s.dialog == nil || s.dialog.WorkspaceID != s.state.ActiveID() {
		return approval.Ticket{}, false
	}
	if runID != "" && s.dialog.RunID != runID {
		return approval.Ticket{}, false
	}
	return *s.dialog, true
}

func (s *shell) submit() {
	if !s.beginWork() {
		return
	}
	ask, err := s.state.Begin(s.state.Draft)
	if err != nil {
		s.work.Done()
		return
	}
	s.mu.Lock()
	s.asks[ask.RunID] = struct{}{}
	s.mu.Unlock()
	// 没有窗口时不能把主线程堵在 Window.Update 上，测试就在当前调用里走完事件流。
	// 有窗口（或测试接了 post）时放到后台，每个事件再回到界面线程。
	if s.win == nil && s.post == nil {
		_ = s.consume(ask, true)
		return
	}
	go s.consume(ask, false)
}

func (s *shell) dropAsk(runID string) {
	s.mu.Lock()
	delete(s.asks, runID)
	s.mu.Unlock()
}

// consume 让 Runner 写入 core/events.Stream，再把读到的事件交给会话状态。
func (s *shell) consume(ask session.Ask, sync bool) error {
	defer s.work.Done()
	defer s.dropAsk(ask.RunID)
	stream := events.NewStream(8)
	errc := make(chan askDone, 1)
	go func() {
		result, err := s.runner.Ask(s.runCtx, session.Ask{
			RunID:       ask.RunID,
			WorkspaceID: ask.WorkspaceID,
			Prompt:      ask.Prompt,
			Events:      stream,
		})
		errc <- askDone{result: result, err: err}
		stream.Close()
	}()
	for ev := range stream.Events() {
		ev := ev
		apply := func() { s.state.Apply(ev) }
		if sync {
			apply()
			continue
		}
		if err := s.deliver(apply); err != nil {
			stream.Close()
			<-errc
			return err
		}
	}
	got := <-errc
	finish := func() {
		s.dropAsk(ask.RunID)
		s.state.FinishResult(ask.RunID, got.err, got.result.RunState)
		s.reloadList()
	}
	if sync {
		finish()
		return got.err
	}
	if derr := s.deliver(finish); derr != nil {
		return derr
	}
	return got.err
}

type askDone struct {
	result session.AskResult
	err    error
}

func (s *shell) deliver(fn func()) error {
	post := s.post
	if s.win != nil {
		post = s.win.Update
	}
	if post == nil {
		fn()
		return nil
	}
	done := make(chan struct{})
	post(func() {
		fn()
		close(done)
	})
	select {
	case <-done:
		return nil
	case <-time.After(5 * time.Second):
		return errors.New("ui: Window.Update 在 5s 内没有回到主线程")
	}
}

func (s *shell) autoAsk(prompt string) error {
	time.Sleep(200 * time.Millisecond)
	if err := s.deliver(func() {
		s.state.Draft = prompt
		s.submit()
	}); err != nil {
		return err
	}
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	var ticketID string
	for ticketID == "" {
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-deadline.C:
			return errors.New("ui: 自动提问在 30s 内没有出现确认单")
		case <-tick.C:
			if err := s.deliver(func() {
				s.mu.Lock()
				defer s.mu.Unlock()
				if s.asking && s.dialog != nil && s.dialog.Action == "export_data" && s.dialog.Risk == "将执行 export_data" && len(s.dialog.ParamsHash) == 64 {
					ticketID = s.dialog.ID
				}
			}); err != nil {
				return err
			}
		}
	}
	if err := s.saveShot(); err != nil {
		return err
	}
	if s.waiter == nil {
		return errors.New("ui: 没有确认等待器")
	}
	if err := s.waiter.Deliver(ticketID, approval.Decision{Action: approval.ActionDefer}); err != nil {
		return err
	}
	for {
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-deadline.C:
			return errors.New("ui: 暂缓后没有保持等待确认")
		case <-tick.C:
			var ready bool
			if err := s.deliver(func() {
				ready = s.state.Phase() == "等待确认"
			}); err != nil {
				return err
			}
			if ready {
				return nil
			}
		}
	}
}

func (s *shell) saveShot() error {
	if s.shot == "" || s.win == nil {
		return nil
	}
	// 完成事件已经写进状态。等主线程把这一帧画完，再抓当前表面。
	// 在 Window.Update 里抓，抓到的是上一帧。
	time.Sleep(400 * time.Millisecond)
	data, err := s.win.CapturePage()
	if err != nil {
		return err
	}
	return os.WriteFile(s.shot, data, 0o644)
}

func (s *shell) confirm(c *mygoui.Context) {
	s.mu.Lock()
	if !s.asking || s.dialog == nil || s.dialog.WorkspaceID != s.state.ActiveID() {
		s.mu.Unlock()
		return
	}
	tk := *s.dialog
	s.mu.Unlock()
	open := true
	message := strings.Join([]string{
		"工具 " + tk.Action,
		session.RiskLine(tk.Risk),
		"哈希 " + tk.ParamsHash,
		"参数 " + string(tk.Params),
		"按 Enter 或 Escape 只暂缓，运行保持挂起。",
	}, "\n")
	// mygo 的 AlertDialog 把最后一个按钮当成默认项：自动聚焦，Enter 点击它。
	// 暂缓放在最后，Enter 不会执行工具。批准只能显式点击。
	choice := mygoui.AlertDialog(c, &open, "确认 "+tk.Action+"？", message, "拒绝", "批准", "暂缓")
	if choice == -1 {
		return
	}
	s.mu.Lock()
	s.asking = false
	s.mu.Unlock()
	if s.waiter == nil {
		return
	}
	var action approval.Action
	switch choice {
	case 0:
		action = approval.ActionReject
	case 1:
		action = approval.ActionApprove
	case mygoui.AlertDismissed, 2:
		action = approval.ActionDefer
	default:
		return
	}
	_ = s.waiter.Deliver(tk.ID, approval.Decision{Action: action})
}

func (s *shell) showTicket(tk approval.Ticket) {
	cp := tk
	apply := func() {
		s.mu.Lock()
		s.dialog = &cp
		s.asking = true
		s.mu.Unlock()
	}
	if s.win == nil && s.post == nil {
		apply()
		return
	}
	_ = s.deliver(apply)
}

// reloadList 只读当前工作区的有效待确认单。
// 不打开工作区，不跑 Recover，也不为了刷新追加恢复审计。
func (s *shell) reloadList() {
	if strings.TrimSpace(s.root) == "" || s.state == nil {
		return
	}
	items, err := session.ReadPending(s.ctx, s.root, s.state.ActiveID(), time.Now())
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.notice = err.Error()
		s.list = nil
		return
	}
	s.list = items
}

func (s *shell) reopen(tk approval.Ticket) {
	if s.waiter == nil || strings.TrimSpace(s.root) == "" {
		return
	}
	if tk.WorkspaceID != s.state.ActiveID() || s.waiter.Busy(tk.RunID) {
		return
	}
	lr, ok := s.runner.(*session.LoopRunner)
	if !ok || lr.Executable() == nil {
		s.setNotice("当前运行器不能恢复确认。")
		return
	}
	tool := lr.Executable()
	if !s.beginWork() {
		return
	}
	s.mu.Lock()
	s.resumes[tk.RunID] = struct{}{}
	s.mu.Unlock()
	go func() {
		defer s.work.Done()
		defer s.dropResume(tk.RunID)
		h, err := workspace.OpenPrepared(s.ctx, s.root, tk.WorkspaceID)
		if err != nil {
			s.setNotice(err.Error())
			return
		}
		defer h.Close()
		svc := run.New(h.Runs)
		svc.SetApprover(s.waiter)
		svc.SetExecutor(tool.Execute)
		res, err := s.waiter.Resume(s.runCtx, svc, tk.RunID)
		s.afterResume(tk.RunID, res, err)
	}()
}

func (s *shell) dropResume(runID string) {
	s.mu.Lock()
	delete(s.resumes, runID)
	s.mu.Unlock()
}

func (s *shell) afterResume(runID string, res run.ResumeResult, err error) {
	fn := func() {
		s.dropResume(runID)
		if s.leaving() {
			return
		}
		switch {
		case errors.Is(err, session.ErrBusy):
			s.notice = "这张确认单正在等待决定。"
		case err == nil && res.State == run.StateCompleted:
			s.notice = "已按冻结参数执行一次。没有继续问模型。"
			s.state.NoteRun(runID, events.DoneCompleted, s.notice)
		case err == nil && res.State == run.StateRejected:
			s.notice = "已拒绝，工具未执行。"
			s.state.NoteRun(runID, events.DoneRejected, s.notice)
		case err == nil && res.State == run.StateAwaitingApproval:
			s.notice = "仍在等待确认。"
		case err != nil:
			s.notice = err.Error()
			s.state.NoteRun(runID, events.DoneFailed, err.Error())
		}
		s.reloadList()
	}
	s.onUI(fn)
}

func (s *shell) setNotice(text string) {
	s.onUI(func() { s.notice = text })
}

func (s *shell) onUI(fn func()) {
	if s.win != nil || s.post != nil {
		_ = s.deliver(fn)
		return
	}
	if s.hops == nil {
		fn()
		return
	}
	s.hops <- fn
}

func (s *shell) drainHops() {
	if s.hops == nil {
		return
	}
	for {
		select {
		case fn := <-s.hops:
			fn()
		default:
			return
		}
	}
}

func latestAssistant(st *session.State) *session.Message {
	th := st.Thread(st.ActiveID())
	for i := len(th.Messages) - 1; i >= 0; i-- {
		if th.Messages[i].Role == session.RoleAssistant {
			return &th.Messages[i]
		}
	}
	return nil
}

// delayTool 在内层工具执行前等 delay，ctx 结束就不再执行。
type delayTool struct {
	tools.Tool
	delay time.Duration
}

func (d delayTool) Execute(ctx context.Context, call tools.Call) (tools.Result, error) {
	timer := time.NewTimer(d.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return tools.Result{}, ctx.Err()
	case <-timer.C:
	}
	return d.Tool.Execute(ctx, call)
}
