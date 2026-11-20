package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	mygoui "github.com/egoist/mygo/ui"
	"github.com/loveRyujin/lantern/core/run"
	"github.com/loveRyujin/lantern/core/workspace"
	"github.com/loveRyujin/lantern/internal/ui/session"
)

// WriteTrace 是命令行的轨迹入口，不打开窗口。
// 先用 OpenPrepared 把库补到当前 schema（不跑 Recover，不写审计），再只读查询。
// 查询失败时返回的错误文案与窗口里一致，不带库里的内容。
func WriteTrace(ctx context.Context, w io.Writer, root, workspaceID, runID string) error {
	if strings.TrimSpace(workspaceID) == "" {
		return errors.New("ui: 需要 -workspace")
	}
	if strings.TrimSpace(root) == "" {
		var err error
		if root, err = workspace.DefaultRoot(); err != nil {
			return err
		}
	}
	h, err := workspace.OpenPrepared(ctx, root, workspaceID)
	if err != nil {
		if errors.Is(err, workspace.ErrNotFound) {
			return fmt.Errorf("工作区 %s 不存在", session.PlainText(workspaceID))
		}
		return err
	}
	if err := h.Close(); err != nil {
		return err
	}
	tr, err := session.ReadTrace(ctx, root, workspaceID, runID)
	if err != nil {
		return errors.New(session.TraceError(workspaceID, runID, err))
	}
	_, err = io.WriteString(w, session.FormatTrace(workspaceID, tr))
	return err
}

const traceRecentLimit = 20

// tracePanel 是运行轨迹视图的状态。只在主线程读写。
// 轨迹每次从 runs.db 只读查询，不缓存其他工作区的结果。
type tracePanel struct {
	open      bool
	workspace string
	input     string
	runID     string
	header    []string
	lines     []session.TraceLine
	recent    []run.RunSummary
	err       string
}

// openTrace 打开轨迹视图。runID 非空时直接查这一次运行。
func (s *shell) openTrace(runID string) {
	ws := s.state.ActiveID()
	s.trace = tracePanel{open: true, workspace: ws, input: runID}
	s.loadRecent()
	if strings.TrimSpace(runID) != "" {
		s.loadTrace(runID)
	}
}

func (s *shell) closeTrace() {
	s.trace = tracePanel{}
}

func (s *shell) loadRecent() {
	if strings.TrimSpace(s.root) == "" {
		s.trace.recent = nil
		return
	}
	list, err := session.ListRuns(s.ctx, s.root, s.trace.workspace, traceRecentLimit)
	if err != nil {
		s.trace.recent = nil
		s.trace.err = session.TraceError(s.trace.workspace, "", err)
		return
	}
	s.trace.recent = list
}

func (s *shell) loadTrace(runID string) {
	runID = strings.TrimSpace(runID)
	s.trace.runID = runID
	s.trace.header = nil
	s.trace.lines = nil
	s.trace.err = ""
	if runID == "" {
		s.trace.err = "请输入运行标识。"
		return
	}
	if strings.TrimSpace(s.root) == "" {
		s.trace.err = "没有数据目录，无法读取轨迹。"
		return
	}
	tr, err := session.ReadTrace(s.ctx, s.root, s.trace.workspace, runID)
	if err != nil {
		s.trace.err = session.TraceError(s.trace.workspace, runID, err)
		return
	}
	s.trace.header = session.TraceHeader(tr)
	s.trace.lines = session.TraceLines(tr)
}

// traceView 占据对话区。所有内容都是 Text，不经过 Markdown。
func (s *shell) traceView(c *mygoui.Context) {
	t := c.Theme()
	if s.trace.workspace != s.state.ActiveID() {
		s.openTrace("")
	}
	mygoui.Column(c).Grow(1).AlignSelf(mygoui.Stretch).Children(func() {
		mygoui.Row(c).Padding(12, 16).Gap(8).AlignItems(mygoui.Center).
			BorderWidth(0, 0, 1, 0).BorderColor(t.Border).Children(func() {
			mygoui.Text(c, "运行轨迹 · "+session.PlainText(s.trace.workspace)).Bold()
			mygoui.Spacer(c)
			if mygoui.Button(c, "返回对话").Clicked() {
				s.closeTrace()
			}
		})
		mygoui.Row(c).Padding(12, 16).Gap(8).AlignItems(mygoui.Center).Children(func() {
			in := mygoui.TextInput(c, &s.trace.input).Placeholder("run_id").Label("轨迹运行标识").Grow(1)
			if mygoui.PrimaryButton(c, "查询轨迹").Clicked() || in.Submitted() {
				s.loadTrace(s.trace.input)
			}
		})
		mygoui.Row(c).Grow(1).AlignItems(mygoui.Stretch).Children(func() {
			s.traceRecent(c)
			mygoui.Scroll(c).Grow(1).Padding(12, 16).Children(func() {
				mygoui.Column(c).Gap(8).Children(func() {
					s.traceBody(c)
				})
			})
		})
	})
}

func (s *shell) traceRecent(c *mygoui.Context) {
	t := c.Theme()
	mygoui.Scroll(c).Width(260).Padding(12, 12).BorderWidth(0, 1, 0, 0).BorderColor(t.Border).Children(func() {
		mygoui.Column(c).Gap(6).Children(func() {
			mygoui.Text(c, "最近运行").Bold()
			if len(s.trace.recent) == 0 {
				mygoui.Text(c, "还没有运行").TextColor(t.TextMuted)
			}
			for _, rs := range s.trace.recent {
				label := session.PlainText(rs.ID) + " · " + session.PlainText(rs.State)
				b := mygoui.Button(c, label)
				if rs.ID == s.trace.runID {
					b = b.Background(t.Accent)
				}
				if b.Clicked() {
					s.trace.input = rs.ID
					s.loadTrace(rs.ID)
				}
			}
		})
	})
}

func (s *shell) traceBody(c *mygoui.Context) {
	t := c.Theme()
	if s.trace.err != "" {
		mygoui.Text(c, s.trace.err).TextColor(t.Danger)
		return
	}
	if len(s.trace.header) == 0 {
		mygoui.Text(c, "输入 run_id，或从左侧选一次运行。只读查询，不改任何记录。").TextColor(t.TextMuted)
		return
	}
	for _, h := range s.trace.header {
		mygoui.Text(c, h)
	}
	mygoui.Text(c, "参数和摘要按纯文本展示，不执行，不渲染 Markdown。").TextColor(t.TextMuted)
	if len(s.trace.lines) == 0 {
		mygoui.Text(c, "这次运行没有步骤记录。").TextColor(t.TextMuted)
	}
	for _, line := range s.trace.lines {
		mygoui.Column(c).Gap(2).Padding(6, 8).Radius(6).Background(t.Surface).Children(func() {
			mygoui.Text(c, line.Time+"  "+line.Head)
			for _, body := range line.Body {
				mygoui.Text(c, body).TextColor(t.TextMuted)
			}
		})
	}
}
