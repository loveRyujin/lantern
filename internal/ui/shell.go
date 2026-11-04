package ui

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/egoist/mygo"
	mygoui "github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown"
	"github.com/loveRyujin/lantern/core/events"
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
	// Runner 把提问写成 core/events。空则用本地脚本，不联网。
	Runner session.Runner
	// CheckPrompt 非空时，窗口起来后自动提这一问，回答结束就退出。
	// 给本机冒烟用。默认测试不设置它，CI 也不会打开窗口。
	CheckPrompt string
	// CheckShot 非空时，自动提问成功后把窗口写成这张 PNG。
	CheckShot string
}

// shell 是主窗口。mygo 类型只留在这个文件所在的包里。
type shell struct {
	win    *mygo.Window
	state  *session.State
	runner session.Runner
	ctx    context.Context
	shot   string
	md     markdown.Options
}

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
	mygo.Theme.SetSource(mygo.ThemeLight)
	errc := make(chan error, 1)
	mygo.App.WhenReady(func() {
		s.win = mygo.NewWindow(mygo.WindowOptions{
			Title:   "lantern",
			Width:   1080,
			Height:  720,
			Content: mygoui.View(s.view),
		})
		if opts.CheckPrompt == "" {
			return
		}
		go func() {
			defer mygo.App.Quit()
			errc <- s.autoAsk(opts.CheckPrompt)
		}()
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
	workspaces := opts.Workspaces
	if len(workspaces) == 0 {
		root := opts.Root
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
	if runner == nil {
		delay := 16 * time.Millisecond
		if opts.CheckPrompt != "" {
			// 冒烟要在超时前走完每个 token 的 Window.Update，间隔短一些。
			delay = 2 * time.Millisecond
		}
		runner = session.ScriptedRunner{Delay: delay}
	}
	s := &shell{state: st, runner: runner, ctx: ctx, shot: opts.CheckShot}
	// AtOnce 让每个 token 在下一帧就出现。负的 Fade 关掉淡入，测试帧才稳定。
	s.md.Reveal = &markdown.Reveal{Unit: markdown.AtOnce, Fade: -1}
	s.md.WrapCode = true
	return s, nil
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
	t := c.Theme()
	mygoui.Column(c).Fill().Children(func() {
		mygoui.Row(c).Padding(12, 16).Gap(12).AlignItems(mygoui.Center).
			BorderWidth(0, 0, 1, 0).BorderColor(t.Border).Children(func() {
			mygoui.Text(c, "lantern").Bold()
			mygoui.Text(c, "mygo v"+ToolkitVersion()).TextColor(t.TextMuted)
			mygoui.Spacer(c)
			mygoui.Text(c, s.state.Phase()).TextColor(t.TextMuted)
		})
		mygoui.Row(c).Grow(1).Children(func() {
			s.sidebar(c)
			s.conversation(c)
		})
	})
}

func (s *shell) sidebar(c *mygoui.Context) {
	t := c.Theme()
	mygoui.Column(c).Width(220).Padding(12, 12).Gap(8).
		BorderWidth(0, 1, 0, 0).BorderColor(t.Border).Children(func() {
		mygoui.Text(c, "工作区").Bold()
		for i, ws := range s.state.Workspaces {
			b := mygoui.Button(c, ws.ID)
			if i == s.state.Active {
				b = b.Background(t.Accent)
			}
			if b.Clicked() {
				s.state.Switch(ws.ID)
			}
		}
		mygoui.Text(c, "会话").TextColor(t.TextMuted)
		mygoui.Text(c, "当前会话")
		if s.localDemo() {
			mygoui.Text(c, "本地演示回答，不联网。").TextColor(t.TextMuted)
		}
	})
}

func (s *shell) conversation(c *mygoui.Context) {
	t := c.Theme()
	th := s.state.Thread(s.state.ActiveID())
	mygoui.Column(c).Grow(1).Children(func() {
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
			busy := s.state.StreamingRun != ""
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
		if m.Note != "" {
			mygoui.Text(c, m.Note).TextColor(t.TextMuted)
		}
	})
}

func emptyDoneLabel(status string) string {
	if status == events.DoneFailed {
		return "这次运行失败。"
	}
	return "没有回复。"
}

func (s *shell) localDemo() bool {
	_, ok := s.runner.(session.ScriptedRunner)
	return ok
}

func (s *shell) submit() {
	ask, err := s.state.Begin(s.state.Draft)
	if err != nil {
		return
	}
	// 没有窗口时不能把主线程堵在 Window.Update 上，测试就在当前调用里走完事件流。
	// 有窗口时放到后台，每个事件再回到主线程。
	if s.win == nil {
		_ = s.consume(ask, true)
		return
	}
	go s.consume(ask, false)
}

// consume 让 Runner 写入 core/events.Stream，再把读到的事件交给会话状态。
func (s *shell) consume(ask session.Ask, sync bool) error {
	stream := events.NewStream(8)
	errc := make(chan error, 1)
	go func() {
		errc <- s.runner.Ask(s.ctx, session.Ask{
			RunID:       ask.RunID,
			WorkspaceID: ask.WorkspaceID,
			Prompt:      ask.Prompt,
			Events:      stream,
		})
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
	err := <-errc
	finish := func() { s.state.Finish(ask.RunID, err) }
	if sync {
		finish()
		return err
	}
	if derr := s.deliver(finish); derr != nil {
		return derr
	}
	return err
}

func (s *shell) deliver(fn func()) error {
	if s.win == nil {
		fn()
		return nil
	}
	done := make(chan struct{})
	s.win.Update(func() {
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
	for {
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-deadline.C:
			return errors.New("ui: 自动提问在 30s 内没有完成")
		case <-tick.C:
			var ready bool
			if err := s.deliver(func() {
				msg := latestAssistant(s.state)
				ready = msg != nil && msg.Done && msg.Status == events.DoneCompleted &&
					strings.Contains(msg.Text, "已确认") && strings.Contains(msg.Text, "SELECT")
			}); err != nil {
				return err
			}
			if ready {
				return s.saveShot()
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

func latestAssistant(st *session.State) *session.Message {
	th := st.Thread(st.ActiveID())
	for i := len(th.Messages) - 1; i >= 0; i-- {
		if th.Messages[i].Role == session.RoleAssistant {
			return &th.Messages[i]
		}
	}
	return nil
}
