package session

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/loveRyujin/lantern/core/events"
)

// Ask 是一次提问。Events 是 core 的类型化事件流，UI 不另造一套事件。
type Ask struct {
	RunID       string
	WorkspaceID string
	Prompt      string
	Events      events.Publisher
}

// Runner 把一次提问写成 token、step、done 事件。
// 确认请求可以不发。发了的话，窗口只把原文留在对话里，不弹确认框。
type Runner interface {
	Ask(ctx context.Context, req Ask) error
}

// ScriptedRunner 在本地拼一段 Markdown，并按 rune 写成 token 事件。
// 它不联网，也不进入 Agent 循环。桌面端在还没接模型时用它把窗口路径跑通。
type ScriptedRunner struct {
	// Delay 是两个 token 之间的等待。测试用 0。
	Delay time.Duration
	// Chunk 是每个 token 事件包含的 rune 数。小于 1 时按 1。
	Chunk int
	// Answer 替换默认文案。返回空字符串时仍走默认文案。
	Answer func(workspaceID, prompt string) string
}

// Ask 依次发布：步骤开始、若干 token、步骤结束、done。
func (r ScriptedRunner) Ask(ctx context.Context, req Ask) error {
	if req.Events == nil {
		return errors.New("session: 事件流为空")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if req.RunID == "" {
		return errors.New("session: 缺少 run_id")
	}
	answer := r.answer(req.WorkspaceID, req.Prompt)
	if err := publish(ctx, req.Events, events.Event{
		Kind:  events.KindStep,
		RunID: req.RunID,
		Step:  &events.Step{ID: "compose", Status: events.StepExecuting, Summary: "整理回答"},
	}); err != nil {
		return err
	}
	for _, part := range chunkRunes(answer, r.Chunk) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if r.Delay > 0 {
			timer := time.NewTimer(r.Delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		if err := publish(ctx, req.Events, events.Event{
			Kind:  events.KindToken,
			RunID: req.RunID,
			Token: &events.Token{Text: part},
		}); err != nil {
			return err
		}
	}
	if err := publish(ctx, req.Events, events.Event{
		Kind:  events.KindStep,
		RunID: req.RunID,
		Step:  &events.Step{ID: "compose", Status: events.StepDone, Summary: "整理回答"},
	}); err != nil {
		return err
	}
	return publish(ctx, req.Events, events.Event{
		Kind:  events.KindDone,
		RunID: req.RunID,
		Done:  &events.Done{Status: events.DoneCompleted, Text: answer},
	})
}

func (r ScriptedRunner) answer(workspaceID, prompt string) string {
	if r.Answer != nil {
		if text := r.Answer(workspaceID, prompt); text != "" {
			return text
		}
	}
	return defaultAnswer(workspaceID, prompt)
}

func defaultAnswer(workspaceID, prompt string) string {
	// 工作区标识贴在句子里，不单独成词，避免和侧边栏按钮的点击文本撞车。
	return fmt.Sprintf("## 回答\n\n已在%s接通。已收到：%s\n\n营收按**已确认**订单统计。\n\n"+
		"| 月份 | 万元 |\n|---|--:|\n| 8 月 | 96 |\n\n"+
		"```sql\nSELECT month, revenue FROM analytics LIMIT 2\n```\n", workspaceID, prompt)
}

func publish(ctx context.Context, pub events.Publisher, ev events.Event) error {
	return pub.Publish(ctx, ev)
}

func chunkRunes(s string, n int) []string {
	if s == "" {
		return nil
	}
	if n < 1 {
		n = 1
	}
	out := make([]string, 0, utf8.RuneCountInString(s)/n+1)
	buf := make([]rune, 0, n)
	flush := func() {
		if len(buf) == 0 {
			return
		}
		out = append(out, string(buf))
		buf = buf[:0]
	}
	for _, r := range s {
		buf = append(buf, r)
		if len(buf) == n {
			flush()
		}
	}
	flush()
	return out
}
