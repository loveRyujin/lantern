package llm

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Mock 按脚本返回结果，供确定性评测和故障注入使用。两次相同脚本、相同请求序列得到相同结果。
type Mock struct {
	script *Script
}

// NewMock 使用 script。script 为 nil 时，Chat 返回 invalid_script。
func NewMock(script *Script) *Mock {
	return &Mock{script: script}
}

func (m *Mock) ID() string { return "mock" }

// Chat 消费脚本的下一步。流式请求会回调 OnDelta；没有 Deltas 时按最终消息合成增量。
// 带故障的步骤可以先发出 Deltas，再返回该故障；返回值不会把这些增量当成成功结果。
func (m *Mock) Chat(ctx context.Context, req Request) (Response, error) {
	if m == nil || m.script == nil {
		return Response{}, invalidScript("mock 没有脚本")
	}
	if err := ctx.Err(); err != nil {
		return Response{}, classifyContext(err)
	}
	req.ToolChoice = strings.TrimSpace(req.ToolChoice)
	if err := validateRequest(req); err != nil {
		return Response{}, err
	}
	step, n, err := m.script.next(req)
	if err != nil {
		return Response{}, err
	}
	if step.Fault != nil {
		if err := waitDelay(ctx, step.Fault.Delay); err != nil {
			return Response{}, err
		}
		if req.wantsStream() {
			for _, delta := range step.Deltas {
				if err := ctx.Err(); err != nil {
					return Response{}, classifyContext(err)
				}
				if req.OnDelta == nil || delta.empty() {
					continue
				}
				if err := req.OnDelta(delta); err != nil {
					return Response{}, err
				}
			}
		}
		if err := ctx.Err(); err != nil {
			return Response{}, classifyContext(err)
		}
		return Response{}, step.Fault.asError()
	}
	msg := step.finalMessage()
	finish := step.finishReason(msg)
	resp := Response{
		ID:           fmt.Sprintf("mock-%d", n),
		Model:        "mock",
		Message:      msg,
		FinishReason: finish,
		Usage:        step.Usage,
	}
	if req.wantsStream() {
		for _, delta := range step.emitDeltas(msg, finish) {
			if err := ctx.Err(); err != nil {
				return Response{}, classifyContext(err)
			}
			if req.OnDelta == nil {
				continue
			}
			if err := req.OnDelta(delta); err != nil {
				return Response{}, err
			}
		}
	}
	return resp, nil
}

func waitDelay(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return classifyContext(ctx.Err())
	case <-timer.C:
		return nil
	}
}

func (s Step) finalMessage() Message {
	content := s.Content
	if content == "" {
		var b strings.Builder
		for _, delta := range s.Deltas {
			b.WriteString(delta.Content)
		}
		content = b.String()
	}
	reasoning := s.ReasoningContent
	if reasoning == "" {
		var b strings.Builder
		for _, delta := range s.Deltas {
			b.WriteString(delta.ReasoningContent)
		}
		reasoning = b.String()
	}
	calls := cloneToolCalls(s.ToolCalls)
	if len(calls) == 0 {
		calls = accumulateDeltaTools(s.Deltas)
	}
	return Message{
		Role:             RoleAssistant,
		Content:          content,
		ReasoningContent: reasoning,
		ToolCalls:        calls,
	}
}

func (s Step) finishReason(msg Message) string {
	if s.FinishReason != "" {
		return s.FinishReason
	}
	if len(msg.ToolCalls) > 0 {
		return FinishToolCalls
	}
	return FinishStop
}

func (s Step) emitDeltas(msg Message, finish string) []Delta {
	if len(s.Deltas) > 0 {
		return cloneDeltas(s.Deltas)
	}
	var out []Delta
	if msg.Content != "" || msg.ReasoningContent != "" {
		out = append(out, Delta{Content: msg.Content, ReasoningContent: msg.ReasoningContent})
	}
	for i, tc := range msg.ToolCalls {
		out = append(out, Delta{ToolCalls: []ToolCallFragment{{
			Index:     i,
			ID:        tc.ID,
			Name:      tc.Name,
			Arguments: tc.Arguments,
		}}})
	}
	if finish != "" {
		out = append(out, Delta{FinishReason: finish})
	}
	return out
}

func accumulateDeltaTools(deltas []Delta) []ToolCall {
	bufs := map[int]*toolBuf{}
	for _, delta := range deltas {
		for _, frag := range delta.ToolCalls {
			buf := toolBufFor(bufs, frag.Index)
			if frag.ID != "" {
				buf.id = frag.ID
			}
			buf.addName(frag.Name)
			buf.args.WriteString(frag.Arguments)
		}
	}
	return toolCallsFrom(bufs)
}

type toolBuf struct {
	id   string
	name strings.Builder
	args strings.Builder
}

func (b *toolBuf) addName(name string) {
	if name != "" && b.name.Len() == 0 {
		b.name.WriteString(name)
	}
}

func toolBufFor(bufs map[int]*toolBuf, index int) *toolBuf {
	buf, ok := bufs[index]
	if !ok {
		buf = &toolBuf{}
		bufs[index] = buf
	}
	return buf
}

func toolCallsFrom(bufs map[int]*toolBuf) []ToolCall {
	if len(bufs) == 0 {
		return nil
	}
	idxs := make([]int, 0, len(bufs))
	for idx := range bufs {
		idxs = append(idxs, idx)
	}
	slices.Sort(idxs)
	out := make([]ToolCall, len(idxs))
	for i, idx := range idxs {
		buf := bufs[idx]
		out[i] = ToolCall{ID: buf.id, Name: buf.name.String(), Arguments: buf.args.String()}
	}
	return out
}
