package ui

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	mygoui "github.com/egoist/mygo/ui"
	"github.com/loveRyujin/lantern/core/events"
	"github.com/loveRyujin/lantern/internal/ui/session"
)

// chartView 是一件图表取回后的结果。sum 是从 Session 取到的字节的 SHA-256。
type chartView struct {
	bitmap *mygoui.Bitmap
	line   string
	sum    string
	open   bool
}

func chartKey(workspaceID, runID, artifactID string) string {
	return workspaceID + "\x00" + runID + "\x00" + artifactID
}

// charts 画一条回答对应运行的图表。只看当前工作区，运行没完成时不画图，避免把半途的产物当结论。
func (s *shell) charts(c *mygoui.Context, m session.Message) {
	if s.analysis == nil || m.RunID == "" || !m.Done {
		return
	}
	ws := s.state.ActiveID()
	rc, ok := s.analysis.Charts(ws, m.RunID)
	if !ok {
		return
	}
	t := c.Theme()
	for _, f := range rc.Failures {
		mygoui.Text(c, session.ChartFailureLine(f)).TextColor(t.Danger)
	}
	if len(rc.Charts) == 0 {
		return
	}
	if m.Status != events.DoneCompleted {
		mygoui.Text(c, "运行没有完成，不展示这次的图表。").TextColor(t.TextMuted)
		return
	}
	for _, ch := range rc.Charts {
		s.chartCard(c, ws, m.RunID, ch)
	}
}

func (s *shell) chartCard(c *mygoui.Context, ws, runID string, ch session.Chart) {
	t := c.Theme()
	v := s.chartFor(ws, runID, ch)
	mygoui.Column(c).Gap(6).Padding(8, 8).Radius(8).BorderWidth(1, 1, 1, 1).BorderColor(t.Border).Children(func() {
		if v.bitmap != nil {
			mygoui.Image(c, v.bitmap).Label(chartLabel(ch)).MaxWidth(600).Fit(mygoui.Contain)
			mygoui.Text(c, ch.Name+" · "+ch.ArtifactID).TextColor(t.TextMuted)
		} else {
			mygoui.Text(c, v.line).TextColor(t.Danger)
		}
		mygoui.Collapsible(c, "SQL 与代码 · "+ch.CallID, &v.open, func() {
			if ch.SQLCallID != "" {
				mygoui.Text(c, "SQL · "+ch.SQLCallID).Bold()
				sql := ch.SQL
				if sql == "" {
					sql = "（这次运行里没有对应的成功 SQL）"
				}
				mygoui.Text(c, sql).Font("monospace").Selectable()
			}
			mygoui.Text(c, "Python · "+ch.CallID).Bold()
			mygoui.Text(c, ch.Code).Font("monospace").Selectable()
		})
	})
}

// chartFor 第一次显示时从 Session 取副本并解码，之后用缓存。切换工作区会清掉缓存，切回时重新取。
func (s *shell) chartFor(ws, runID string, ch session.Chart) *chartView {
	key := chartKey(ws, runID, ch.ArtifactID)
	if v := s.chartViews[key]; v != nil {
		return v
	}
	v := &chartView{}
	art, err := s.analysis.Fetch(ws, runID, ch.ArtifactID)
	if err == nil {
		v.bitmap, err = mygoui.DecodeBitmap(art.Data)
		if err != nil {
			err = errors.New("图表无法解码")
			v.bitmap = nil
		} else {
			sum := sha256.Sum256(art.Data)
			v.sum = hex.EncodeToString(sum[:])
		}
	}
	if err != nil {
		v.line = session.ChartErrorLine(ch.Name, err)
	}
	if s.chartViews == nil {
		s.chartViews = map[string]*chartView{}
	}
	s.chartViews[key] = v
	return v
}

func chartLabel(ch session.Chart) string {
	return "图表 " + ch.Name + " · " + ch.ArtifactID
}

// dropCharts 丢掉已解码的图表。下次显示时重新向 Session 取，Session 已关闭就给出明确提示。
func (s *shell) dropCharts() {
	s.chartViews = nil
}

// autoChart 是 -check 的出图版本：提问，等回答完成且图表已画出，再截图。
func (s *shell) autoChart(prompt string) error {
	time.Sleep(200 * time.Millisecond)
	if err := s.deliver(func() {
		s.state.Draft = prompt
		s.submit()
	}); err != nil {
		return err
	}
	deadline := time.NewTimer(3 * time.Minute)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-deadline.C:
			return errors.New("ui: 自动提问在 3 分钟内没有画出图表")
		case <-tick.C:
		}
		var drawn bool
		var why string
		if err := s.deliver(func() {
			m := latestAssistant(s.state)
			if m == nil || !m.Done {
				return
			}
			for _, v := range s.chartViews {
				drawn = drawn || v.bitmap != nil
			}
			rc, _ := s.analysis.Charts(s.state.ActiveID(), m.RunID)
			switch {
			case m.Status != events.DoneCompleted:
				why = "运行状态 " + m.Status
			case len(rc.Charts) == 0 && len(rc.Failures) > 0:
				why = session.ChartFailureLine(rc.Failures[len(rc.Failures)-1])
			case len(rc.Charts) == 0:
				why = "运行完成但没有产物"
			}
		}); err != nil {
			return err
		}
		if drawn || why != "" {
			if err := s.saveShot(); err != nil {
				return err
			}
			if !drawn {
				return errors.New("ui: 没有画出图表：" + why)
			}
			return nil
		}
	}
}
