package ui

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	mygoui "github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown"
)

func newSpikeTester(s *spike) *mygoui.Tester {
	return mygoui.NewTester(s.view, 980, 700)
}

func TestSpikeMarkdownPrefix(t *testing.T) {
	if !strings.HasPrefix(spikeMarkdown, spikeMarkdownPrefix) {
		t.Fatal("截断点不是全文的前缀")
	}
	if !strings.Contains(spikeMarkdownPrefix, "**已确") || strings.Contains(spikeMarkdownPrefix, "订单统计") {
		t.Fatalf("截断点应落在未闭合的强调里，prefix=%q", spikeMarkdownPrefix)
	}
}

func TestSpikeStreamHealsEmphasis(t *testing.T) {
	s := newSpike()
	s.instantReveal()
	s.reply = spikeMarkdownPrefix
	s.streaming = true

	tt := newSpikeTester(s)
	tt.Frame()
	if !tt.HasText("口径核对") || !tt.HasText("已确") || !tt.HasText("营收按") {
		t.Fatalf("流式文本没有画出来：%q", tt.Texts())
	}
	if tt.HasText("**") || tt.HasText("订单统计") || tt.HasText("8 月") || tt.HasText("SELECT") {
		t.Fatalf("未闭合标记或后文漏进了画面：%q", tt.Texts())
	}
}

func TestSpikeStreamHidesOpenSentence(t *testing.T) {
	s := newSpike()
	s.md.Reveal.Fade = -1
	s.md.Reveal.CatchUp = time.Millisecond
	s.reply = "营收按已确认订单统计。未确认的还不"
	s.streaming = true

	tt := newSpikeTester(s)
	tt.Frame()
	tt.Advance(time.Second)
	got := strings.Join(tt.Texts(), "")
	if !strings.Contains(got, "营收按已确认订单统计。") {
		t.Fatalf("完整句没有出现：%s", got)
	}
	if strings.Contains(got, "未确认") {
		t.Fatalf("BySentence 把未写完的句子也显示了：%s", got)
	}

	s.done = true
	s.streaming = false
	tt.Frame()
	tt.Advance(time.Second)
	if !tt.HasText("未确认") {
		t.Fatalf("完成后 View 仍藏着后半句：%q", tt.Texts())
	}
}

func TestSpikeDoneShowsDocument(t *testing.T) {
	s := newSpike()
	s.instantReveal()
	s.reply = spikeMarkdown
	s.done = true

	tt := newSpikeTester(s)
	tt.Frame()
	for _, want := range []string{"口径", "8 月", "128", "只读查询", "SELECT", "revenue", "敏感库"} {
		if !tt.HasText(want) {
			t.Errorf("完成稿缺少 %q，现有 %q", want, tt.Texts())
		}
	}
}

func TestSpikeFeedAppliesEachRune(t *testing.T) {
	s := newSpike()
	if err := s.feed(spikeMarkdown, nil); err != nil {
		t.Fatal(err)
	}
	if !s.done || s.streaming || s.reply != spikeMarkdown {
		t.Fatalf("done=%v streaming=%v reply=%q", s.done, s.streaming, s.reply)
	}
	want := utf8.RuneCountInString(spikeMarkdown) + 1
	if s.updates != want {
		t.Fatalf("updates=%d，想要每个 rune 一次再加完成一次（%d）", s.updates, want)
	}
}

func TestSpikeAlertEscapeDefers(t *testing.T) {
	s := newSpike()
	s.asking = true
	tt := newSpikeTester(s)
	tt.Frame()
	if !tt.HasText("执行 export_data？") || !tt.HasText("敏感库") {
		t.Fatalf("确认框没有出现：%q", tt.Texts())
	}
	tt.Key(0, mygoui.KeyEscape)
	if s.asking || s.decision != "暂缓" {
		t.Fatalf("Escape 之后 asking=%v decision=%q", s.asking, s.decision)
	}
	if tt.HasText("执行 export_data？") {
		t.Fatalf("Escape 后确认框还在：%q", tt.Texts())
	}
}

func TestSpikeAlertButtons(t *testing.T) {
	for _, want := range []string{"拒绝", "批准"} {
		s := newSpike()
		s.asking = true
		tt := newSpikeTester(s)
		tt.Frame()
		if err := tt.Click(want); err != nil {
			t.Fatal(err)
		}
		if s.asking || s.decision != want {
			t.Fatalf("点击 %q 之后 asking=%v decision=%q", want, s.asking, s.decision)
		}
	}
}

func TestSpikeChart(t *testing.T) {
	s := newSpike()
	tt := newSpikeTester(s)
	tt.Frame()
	for _, step := range spikeSteps {
		if !tt.HasText(step.Name) {
			t.Errorf("坐标轴缺少 %q，现有 %q", step.Name, tt.Texts())
		}
	}
	if err := tt.Click("检索柱"); err != nil {
		t.Fatal(err)
	}
	if s.picked != 0 || !tt.HasText("选中：检索 42 ms") {
		t.Fatalf("picked=%d texts=%q", s.picked, tt.Texts())
	}
}

func TestSpikeRevealDefaultIsSentence(t *testing.T) {
	s := newSpike()
	if s.md.Reveal == nil || s.md.Reveal.Unit != markdown.BySentence {
		t.Fatalf("默认 Reveal = %+v", s.md.Reveal)
	}
}
