package ui

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"time"

	mygoui "github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/chart"
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

// charts 画一条回答对应运行的图表：先是每次成功 run_sql 的结构化柱状图，再是沙箱交回的 PNG。
// 只看当前工作区，运行没完成时不画图，避免把半途的产物当结论。
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
	if len(rc.Tables) == 0 && len(rc.Charts) == 0 {
		return
	}
	if m.Status != events.DoneCompleted {
		mygoui.Text(c, "运行没有完成，不展示这次的图表。").TextColor(t.TextMuted)
		return
	}
	for _, tc := range rc.Tables {
		s.tableCard(c, ws, m.RunID, tc)
	}
	for _, ch := range rc.Charts {
		s.chartCard(c, ws, m.RunID, ch, rc.Closed)
	}
}

func (s *shell) chartCard(c *mygoui.Context, ws, runID string, ch session.Chart, closed bool) {
	t := c.Theme()
	v := s.chartFor(ws, runID, ch, closed)
	mygoui.Column(c).Gap(6).Padding(8, 8).Radius(8).BorderWidth(1, 1, 1, 1).BorderColor(t.Border).Children(func() {
		if v.bitmap != nil {
			mygoui.Image(c, v.bitmap).Label(chartLabel(ch)).MaxWidth(600).Fit(mygoui.Contain)
			mygoui.Text(c, "沙箱图片 · "+ch.Name+" · "+ch.ArtifactID).TextColor(t.TextMuted)
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

// tableCard 用 ui/chart 画一次 run_sql 的结果。坐标只由工具结果里的数值算出；不能画时只给原因。
func (s *shell) tableCard(c *mygoui.Context, ws, runID string, tc session.SQLChart) {
	t := c.Theme()
	key := chartKey(ws, runID, "sql\x00"+tc.CallID)
	v := s.chartViews[key]
	if v == nil {
		v = &chartView{}
		if s.chartViews == nil {
			s.chartViews = map[string]*chartView{}
		}
		s.chartViews[key] = v
	}
	mygoui.Column(c).Gap(6).Padding(8, 8).Radius(8).BorderWidth(1, 1, 1, 1).BorderColor(t.Border).Children(func() {
		sr := tc.Series
		var y chart.Linear
		var ticks []chart.Tick
		if sr != nil {
			var reason string
			if y, ticks, reason = barScale(sr.Values); reason != "" {
				tc.Code, tc.Reason, sr = session.CodeSeriesNotPlottable, reason, nil
			}
		}
		if sr == nil {
			mygoui.Text(c, session.SeriesLine(tc)).TextColor(t.TextMuted)
		} else {
			mygoui.Text(c, fmt.Sprintf("%s 按 %s · run_sql %s", sr.YName, sr.XName, tc.CallID)).TextColor(t.TextMuted)
			if sr.Truncated {
				reason := sr.TruncateReason
				if reason == "" {
					reason = "limit"
				}
				mygoui.Text(c, fmt.Sprintf("结果已截断（%s），图只含返回的 %d 行", reason, len(sr.Values))).TextColor(t.Danger)
			}
			seriesPlot(c, tc.CallID, sr, y, ticks)
		}
		mygoui.Collapsible(c, "SQL · "+tc.CallID, &v.open, func() {
			mygoui.Text(c, tc.SQL).Font("monospace").Selectable()
		})
	})
}

// maxTickLabel 是纵轴刻度文字的最大长度。再长，刻度会把 600 宽的绘图区挤没。
const maxTickLabel = 24

// barScale 给柱状图定纵轴：含 0，经 Nice 取整。值域溢出、柱的位置算不出有限值，
// 或刻度文字长到挤掉绘图区时返回原因，调用方不画任何坐标。
func barScale(values []float64) (chart.Linear, []chart.Tick, string) {
	lo, hi := chart.Extent(values...)
	lo, hi = min(lo, 0), max(hi, 0)
	if lo == hi {
		hi = lo + 1
	}
	y := chart.NewLinear(lo, hi).Nice(4)
	if span := y.Hi - y.Lo; !finite(y.Lo) || !finite(y.Hi) || !finite(span) || span <= 0 {
		return y, nil, "数值范围太大，纵轴无法取整"
	}
	for _, v := range values {
		if at := float64(y.At(v)); !finite(at) || at < 0 || at > 1 {
			return y, nil, "数值超出纵轴，柱的位置算不出来"
		}
	}
	ticks := y.Ticks(4)
	if len(ticks) < 2 {
		return y, nil, "纵轴刻度不足"
	}
	for _, tk := range ticks {
		if len(tk.Label) > maxTickLabel {
			return y, nil, fmt.Sprintf("纵轴刻度 %d 位，绘图区放不下", len(tk.Label))
		}
	}
	return y, ticks, ""
}

func seriesValues(sr *session.Series) []float64 {
	if sr == nil {
		return nil
	}
	return sr.Values
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

func seriesPlot(c *mygoui.Context, callID string, sr *session.Series, y chart.Linear, ticks []chart.Tick) {
	t := c.Theme()
	x := chart.NewBand(sr.Labels...).Padding(0.28, 0.12)
	chart.Plot(c, chart.Axes{
		Left:   ticks,
		Bottom: x.Ticks(),
		Label: func(_ chart.Side, tick chart.Tick) {
			mygoui.Text(c, tick.Label).NoWrap().FontSize(11).TextColor(t.TextMuted)
		},
	}, func(area mygoui.Element) {
		area.Label(seriesLabel(callID)).Draw(func(p *mygoui.Painter, r mygoui.Rect) {
			for _, tick := range ticks {
				p.Line(r.X, chart.Y(r, tick.At), r.X+r.W, chart.Y(r, tick.At), 1, t.Border)
			}
		})
		for i, v := range sr.Values {
			chart.Rect(c, x.Band(i), y.Span(0, v)).Radius(4).Background(t.Accent).
				Label(barLabel(sr.Labels[i], sr.Raw[i]))
		}
	}).Height(180).Gap(6).MaxWidth(600)
}

func seriesLabel(callID string) string { return "结构化图表 · " + callID }

func barLabel(label, raw string) string { return "柱 " + label + " = " + raw }

// chartFor 第一次显示时从 Session 取副本并解码，之后用缓存。切换工作区会清掉缓存，切回时重新取。
// Session 被淘汰或关闭后丢掉缓存的位图，和切回后看到的一致。
func (s *shell) chartFor(ws, runID string, ch session.Chart, closed bool) *chartView {
	key := chartKey(ws, runID, ch.ArtifactID)
	if v := s.chartViews[key]; v != nil && !(closed && v.bitmap != nil) {
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

// autoChart 是 -check 的出图版本：提问，等回答完成且图表已画出，再截图。有 PNG 时等它解码画出，否则等结构化图表。
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
			ws := s.state.ActiveID()
			rc, _ := s.analysis.Charts(ws, m.RunID)
			for _, tc := range rc.Tables {
				_, _, refused := barScale(seriesValues(tc.Series))
				drawn = drawn || (tc.Series != nil && refused == "" && s.chartViews[chartKey(ws, m.RunID, "sql\x00"+tc.CallID)] != nil)
			}
			if len(rc.Charts) > 0 {
				drawn = false
				for _, v := range s.chartViews {
					drawn = drawn || v.bitmap != nil
				}
			}
			switch {
			case m.Status != events.DoneCompleted:
				why = "运行状态 " + m.Status
			case !drawn && len(rc.Charts) == 0 && len(rc.Failures) > 0:
				why = session.ChartFailureLine(rc.Failures[len(rc.Failures)-1])
			case len(rc.Charts) == 0 && len(rc.Tables) == 0:
				why = "运行完成但没有图表"
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
