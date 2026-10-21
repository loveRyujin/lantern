package ui

import (
	"bytes"
	"errors"
	"fmt"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	"github.com/egoist/mygo"
	mygoui "github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/chart"
	"github.com/egoist/mygo/ui/markdown"
)

// 流式回复在「已确」两个字中间截断，用来看 markdown.Stream 怎么补全未闭合的强调。
const spikeMarkdownPrefix = "## 口径核对\n\n营收按**已确"

const spikeMarkdown = spikeMarkdownPrefix + "认**订单统计。未确认的还不计入。\n\n" +
	"详见 [口径](https://example.invalid/metric)。\n\n" +
	"| 月份 | 万元 |\n" +
	"|---|--:|\n" +
	"| 8 月 | 96 |\n" +
	"| 9 月 | 128 |\n\n" +
	"> 只读查询，未打开敏感库。\n\n" +
	"```sql\nSELECT month, revenue FROM analytics LIMIT 2\n```\n"

// spikeSteps 是图表里的四根柱，数值固定，截图可以对照。
var spikeSteps = []struct {
	Name string
	MS   float64
}{
	{"检索", 42},
	{"SQL", 18},
	{"确认", 310},
	{"生成", 128},
}

// SpikeOptions 控制最小窗口怎么跑。
type SpikeOptions struct {
	// ShotsDir 非空时，窗口起来后自动把回复流式写完，抓三张 PNG，然后退出。
	ShotsDir string
}

// spike 是 mygo v0.6.0 的最小窗口：流式 Markdown、确认框、柱状图，
// 以及从后台 goroutine 调用 Window.Update。它不是产品壳，cmd/lantern 不进入这里。
type spike struct {
	win *mygo.Window

	md         markdown.Options
	reply      string
	streaming  bool
	done       bool
	updates    int
	asking     bool
	decision   string
	picked     int
	tokenDelay time.Duration
}

func newSpike() *spike {
	s := &spike{picked: -1}
	// 中文句子以「。」结束。ByWord 只在空格和换行处截断，一段没有空格的中文会整段当作一个词。
	s.md.Reveal = &markdown.Reveal{
		Unit:    markdown.BySentence,
		CatchUp: 200 * time.Millisecond,
		Fade:    160 * time.Millisecond,
	}
	s.md.WrapCode = true
	return s
}

// instantReveal 让缓冲区里的文本在下一帧全部出现。截图要避开淡入的半透明中间帧。
func (s *spike) instantReveal() {
	s.md.Reveal = &markdown.Reveal{Unit: markdown.AtOnce, Fade: -1}
}

// RunSpike 打开最小窗口并进入事件循环。
func RunSpike(opts SpikeOptions) error {
	s := newSpike()
	if opts.ShotsDir != "" {
		s.instantReveal()
	}
	mygo.Theme.SetSource(mygo.ThemeLight)

	errc := make(chan error, 1)
	mygo.App.WhenReady(func() {
		s.win = mygo.NewWindow(mygo.WindowOptions{
			Title:   "lantern · mygo v" + ToolkitVersion(),
			Width:   980,
			Height:  700,
			Content: mygoui.View(s.view),
		})
		if opts.ShotsDir == "" {
			return
		}
		go func() {
			defer mygo.App.Quit()
			defer func() {
				if rec := recover(); rec != nil {
					errc <- fmt.Errorf("spike panic: %v\n%s", rec, debug.Stack())
				}
			}()
			errc <- s.captureShots(opts.ShotsDir)
		}()
	})
	if err := mygo.App.Run(); err != nil {
		return err
	}
	if opts.ShotsDir == "" {
		return nil
	}
	select {
	case err := <-errc:
		return err
	default:
		return errors.New("截图过程没有返回结果")
	}
}

func (s *spike) view(c *mygoui.Context) {
	t := c.Theme()
	mygoui.Column(c).Fill().Children(func() {
		mygoui.Row(c).Padding(12, 16).Gap(12).AlignItems(mygoui.Center).
			BorderWidth(0, 0, 1, 0).BorderColor(t.Border).Children(func() {
			mygoui.Text(c, "lantern").Bold()
			mygoui.Text(c, "mygo v"+ToolkitVersion()).TextColor(t.TextMuted)
			mygoui.Spacer(c)
			mygoui.Text(c, s.status()).TextColor(t.TextMuted)
		})

		mygoui.Row(c).Padding(10, 16).Gap(8).AlignItems(mygoui.Center).Children(func() {
			if mygoui.Button(c, "流式回复").Disabled(s.streaming).Clicked() {
				s.startStream()
			}
			if mygoui.Button(c, "请求确认").Clicked() {
				s.asking = true
			}
			if s.decision != "" {
				mygoui.Text(c, "确认："+s.decision).TextColor(t.TextMuted)
			}
		})

		mygoui.Scroll(c).Grow(1).Padding(8, 16).Children(func() {
			s.markdown(c)
		})

		mygoui.Column(c).Padding(8, 16, 12, 16).Gap(6).
			BorderWidth(1, 0, 0, 0).BorderColor(t.Border).Children(func() {
			s.bars(c)
		})
	})

	switch mygoui.AlertDialog(c, &s.asking,
		"执行 export_data？",
		"确认后才会打开敏感库。按 Escape 视为暂缓。",
		"暂缓", "拒绝", "批准") {
	case mygoui.AlertDismissed, 0:
		s.decision = "暂缓"
	case 1:
		s.decision = "拒绝"
	case 2:
		s.decision = "批准"
	}
}

func (s *spike) status() string {
	phase := "等待"
	switch {
	case s.asking:
		phase = "等待确认"
	case s.streaming:
		phase = "流式中"
	case s.done:
		phase = "已完成"
	}
	return fmt.Sprintf("%s · 后台 Window.Update %d 次", phase, s.updates)
}

func (s *spike) markdown(c *mygoui.Context) {
	if s.reply == "" {
		mygoui.Text(c, "等待回复…").TextColor(c.Theme().TextMuted)
		return
	}
	if s.done {
		markdown.View(c, s.reply, &s.md)
		return
	}
	markdown.Stream(c, s.reply, &s.md)
}

func (s *spike) bars(c *mygoui.Context) {
	t := c.Theme()
	names := make([]string, len(spikeSteps))
	for i, step := range spikeSteps {
		names[i] = step.Name
	}
	x := chart.NewBand(names...).Padding(0.28, 0.12)
	y := chart.NewLinear(0, 360).Nice(4)
	ticks := y.Ticks(4)

	caption := "步骤耗时（毫秒）。点击柱形查看数值。"
	if s.picked >= 0 {
		step := spikeSteps[s.picked]
		caption = fmt.Sprintf("选中：%s %.0f ms", step.Name, step.MS)
	}
	mygoui.Text(c, caption).TextColor(t.TextMuted)

	chart.Plot(c, chart.Axes{
		Left:   ticks,
		Bottom: x.Ticks(),
		Label: func(_ chart.Side, tick chart.Tick) {
			mygoui.Text(c, tick.Label).NoWrap().FontSize(11).TextColor(t.TextMuted)
		},
	}, func(area mygoui.Element) {
		area.Label("步骤耗时").Draw(func(p *mygoui.Painter, r mygoui.Rect) {
			for _, tick := range ticks {
				p.Line(r.X, chart.Y(r, tick.At), r.X+r.W, chart.Y(r, tick.At), 1, t.Border)
			}
		})
		for i, step := range spikeSteps {
			bar := chart.Rect(c, x.Band(i), y.Span(0, step.MS)).
				Radius(4).Background(t.Accent).Label(step.Name + "柱")
			if bar.Clicked() {
				s.picked = i
			}
		}
	}).Height(168).Gap(6)
}

// startStream 在后台 goroutine 里把回复逐字送进窗口。点击处理跑在主线程上，
// 这里只启动 goroutine，不在主线程上等待 Window.Update。
func (s *spike) startStream() {
	if s.streaming {
		return
	}
	s.streaming = true
	s.done = false
	s.reply = ""
	s.tokenDelay = 16 * time.Millisecond
	go func() { _ = s.feed(spikeMarkdown, nil) }()
}

// feed 按 rune 追加回复。每个 rune 都经过 mustUpdate，因此有窗口时发生在主线程。
func (s *spike) feed(src string, after func(text string) error) error {
	var buf bytes.Buffer
	for _, r := range src {
		buf.WriteRune(r)
		text := buf.String()
		if err := s.mustUpdate(func() {
			s.reply = text
			s.streaming = true
			s.done = false
			s.updates++
		}); err != nil {
			return err
		}
		if after != nil {
			if err := after(text); err != nil {
				return err
			}
		}
		if s.tokenDelay > 0 {
			time.Sleep(s.tokenDelay)
		}
	}
	return s.mustUpdate(func() {
		s.reply = src
		s.done = true
		s.streaming = false
		s.updates++
	})
}

// mustUpdate 在有窗口时把 fn 交给 Window.Update，并等到它在主线程跑完。
// 没有窗口时（测试）就在当前 goroutine 里直接跑。
func (s *spike) mustUpdate(fn func()) error {
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
		return errors.New("Window.Update 在 5s 内没有回到主线程")
	}
}

func (s *spike) captureShots(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// 等第一帧把表面建起来，后面的 Update 才有内容可画。
	time.Sleep(300 * time.Millisecond)

	var streamPNG, donePNG, alertPNG []byte
	err := s.feed(spikeMarkdown, func(text string) error {
		if text != spikeMarkdownPrefix || streamPNG != nil {
			return nil
		}
		if err := s.settle(); err != nil {
			return err
		}
		var err error
		streamPNG, err = s.grab("stream")
		return err
	})
	if err != nil {
		return err
	}
	if streamPNG == nil {
		return errors.New("流式截图没有抓到截断点")
	}
	if err := s.settle(); err != nil {
		return err
	}
	donePNG, err = s.grab("done")
	if err != nil {
		return err
	}
	if err := s.mustUpdate(func() { s.asking = true }); err != nil {
		return err
	}
	if err := s.settle(); err != nil {
		return err
	}
	alertPNG, err = s.grab("alert")
	if err != nil {
		return err
	}
	if bytes.Equal(streamPNG, donePNG) || bytes.Equal(donePNG, alertPNG) {
		return errors.New("三张截图里有两张完全相同，画面没有跟上状态")
	}

	names := []struct {
		name string
		png  []byte
	}{
		{"stream.png", streamPNG},
		{"done.png", donePNG},
		{"alert.png", alertPNG},
	}
	for _, shot := range names {
		if err := os.WriteFile(filepath.Join(dir, shot.name), shot.png, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "spike: %s %d bytes\n", shot.name, len(shot.png))
	}
	fmt.Fprintf(os.Stderr, "spike: version=%s updates=%d asking=%v\n", ToolkitVersion(), s.updates, s.asking)
	return nil
}

func (s *spike) settle() error {
	time.Sleep(400 * time.Millisecond)
	return nil
}

func (s *spike) grab(name string) ([]byte, error) {
	data, err := s.win.CapturePage()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if err := assertPicture(data); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return data, nil
}

func assertPicture(data []byte) error {
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return err
	}
	b := img.Bounds()
	if b.Dx() < 400 || b.Dy() < 300 {
		return fmt.Errorf("尺寸 %dx%d，不像一扇窗口", b.Dx(), b.Dy())
	}
	fr, fg, fb := colorOf(img.At(b.Min.X, b.Min.Y))
	diff := 0
	for y := b.Min.Y; y < b.Max.Y; y += 4 {
		for x := b.Min.X; x < b.Max.X; x += 4 {
			r, g, bl := colorOf(img.At(x, y))
			if abs(r-fr) > 12 || abs(g-fg) > 12 || abs(bl-fb) > 12 {
				diff++
			}
		}
	}
	if diff < 50 {
		return fmt.Errorf("画面近乎纯色（%d 个采样点和背景不同）", diff)
	}
	return nil
}

func colorOf(c color.Color) (r, g, b int) {
	rr, gg, bb, _ := c.RGBA()
	return int(rr >> 8), int(gg >> 8), int(bb >> 8)
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
