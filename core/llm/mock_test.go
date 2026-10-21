package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

const sampleScript = `{
  "steps": [
    {
      "content": "你好",
      "deltas": [{"content": "你"}, {"content": "好"}],
      "finish_reason": "stop",
      "usage": {"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}
    },
    {
      "tool_calls": [
        {"id": "call_1", "name": "run_sql", "arguments": "{\"query\":\"SELECT 1\"}"},
        {"id": "call_2", "name": "export_data", "arguments": "{not json"}
      ],
      "finish_reason": "tool_calls"
    },
    {"fault": {"kind": "timeout"}},
    {"fault": {"kind": "http_5xx", "status": 503, "message": "unavailable"}},
    {"fault": {"kind": "invalid_json"}},
    {"fault": {"kind": "ollama_unreachable"}}
  ]
}`

type traceItem struct {
	Resp      Response
	Deltas    []Delta
	ErrCode   string
	ErrText   string
	Retryable bool
}

func TestMockDeterministicScript(t *testing.T) {
	left := collectScript(t, []byte(sampleScript))
	right := collectScript(t, []byte(sampleScript))
	if !reflect.DeepEqual(left, right) {
		t.Fatalf("two runs differ\nleft %#v\nright %#v", left, right)
	}
	if len(left) != 7 {
		t.Fatalf("trace len %d", len(left))
	}
	if left[0].Resp.Message.Content != "你好" || left[0].Resp.ID != "mock-1" || left[0].Resp.FinishReason != FinishStop {
		t.Fatalf("text step %#v", left[0])
	}
	if got := deltaText(left[0].Deltas); got != "你好" {
		t.Fatalf("deltas %q", got)
	}
	if left[0].Resp.Usage.TotalTokens != 5 {
		t.Fatalf("usage %#v", left[0].Resp.Usage)
	}
	calls := left[1].Resp.Message.ToolCalls
	if len(calls) != 2 || calls[0].Name != "run_sql" || calls[1].Arguments != "{not json" {
		t.Fatalf("tool calls %#v", calls)
	}
	if left[1].Resp.ID != "mock-2" || left[1].Resp.FinishReason != FinishToolCalls {
		t.Fatalf("tool response %#v", left[1].Resp)
	}
	assertFault(t, left[2], CodeTimeout, true)
	assertFault(t, left[3], CodeLLM5xx, true)
	if !strings.Contains(left[3].ErrText, "HTTP 503") || !strings.Contains(left[3].ErrText, "unavailable") {
		t.Fatalf("5xx text %q", left[3].ErrText)
	}
	assertFault(t, left[4], CodeInvalidJSON, false)
	assertFault(t, left[5], CodeOllamaUnreachable, false)
	if !strings.Contains(left[5].ErrText, "localhost:11434") {
		t.Fatalf("ollama text %q", left[5].ErrText)
	}
	assertFault(t, left[6], CodeScriptExhausted, false)

	again, err := ParseScript([]byte(sampleScript))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(again)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip := collectScript(t, raw)
	if !reflect.DeepEqual(left, roundTrip) {
		t.Fatalf("round trip differ\n%#v\n%#v", left, roundTrip)
	}
}

func TestMockWhenMismatchIsConsumed(t *testing.T) {
	script := MustNewScript(Step{
		When:    &When{Contains: "GMV", Tool: "run_sql", Role: RoleUser},
		Content: "口径",
	})
	mock := NewMock(script)
	req := Request{Messages: []Message{{Role: RoleUser, Content: "hello"}}}
	_, err := mock.Chat(context.Background(), req)
	if !errors.Is(err, ErrScriptMismatch) {
		t.Fatal(err)
	}
	if !strings.Contains(err.Error(), "GMV") || !strings.Contains(err.Error(), "run_sql") {
		t.Fatal(err)
	}
	_, err = mock.Chat(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Content: "GMV"}},
		Tools:    []Tool{{Name: "run_sql"}},
	})
	if !errors.Is(err, ErrScriptExhausted) {
		t.Fatalf("mismatch should consume the step, got %v", err)
	}

	script.Reset()
	resp, err := mock.Chat(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Content: "查 GMV"}},
		Tools:    []Tool{{Name: "run_sql"}},
	})
	if err != nil || resp.Message.Content != "口径" {
		t.Fatalf("reset resp %#v %v", resp, err)
	}
}

func TestMockStreamAndIsolation(t *testing.T) {
	call := ToolCall{ID: "call_1", Name: "run_sql", Arguments: `{"query":"SELECT 1"}`}
	script := MustNewScript(CallTools(call))
	mock := NewMock(script)
	var deltas []Delta
	resp, err := mock.Chat(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Content: "q"}},
		OnDelta: func(d Delta) error {
			if len(d.ToolCalls) > 0 {
				d.ToolCalls[0].Arguments = "mutated"
			}
			deltas = append(deltas, d)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Message.ToolCalls[0].Arguments = "mutated"
	resp.Message.ToolCalls[0].Name = "nope"
	script.Reset()
	again, err := mock.Chat(context.Background(), Request{
		Stream:   true,
		Messages: []Message{{Role: RoleUser, Content: "q"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.Message.ToolCalls[0].Arguments != `{"query":"SELECT 1"}` || again.Message.ToolCalls[0].Name != "run_sql" {
		t.Fatalf("script was mutated: %#v", again.Message.ToolCalls)
	}
	if len(deltas) < 2 {
		t.Fatalf("deltas %#v", deltas)
	}
	foundCall := false
	foundFinish := false
	for _, d := range deltas {
		if len(d.ToolCalls) == 1 && d.ToolCalls[0].Name == "run_sql" {
			foundCall = true
		}
		if d.FinishReason == FinishToolCalls {
			foundFinish = true
		}
	}
	if !foundCall || !foundFinish {
		t.Fatalf("synthetic deltas %#v", deltas)
	}
}

func TestMockOnDeltaErrorAndCancelDuringDelay(t *testing.T) {
	sentinel := errors.New("stop")
	mock := NewMock(MustNewScript(Say("a", "b")))
	_, err := mock.Chat(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Content: "q"}},
		OnDelta: func(Delta) error {
			return sentinel
		},
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("ondelta: %v", err)
	}

	mock = NewMock(MustNewScript(Step{Fault: &Fault{Kind: FaultHTTP5xx, Status: 503, Delay: 2 * time.Second}}))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = mock.Chat(ctx, Request{Messages: []Message{{Role: RoleUser, Content: "q"}}})
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrLLM5xx) {
		t.Fatalf("delay cancel: %v", err)
	}
}

func TestMockExplicitContentWinsOverDeltas(t *testing.T) {
	mock := NewMock(MustNewScript(Step{
		Content: "AB",
		Deltas:  []Delta{{Content: "A"}, {Content: "B"}, {Content: "C"}},
	}))
	var got strings.Builder
	resp, err := mock.Chat(context.Background(), Request{
		Stream:   true,
		Messages: []Message{{Role: RoleUser, Content: "q"}},
		OnDelta: func(d Delta) error {
			got.WriteString(d.Content)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Content != "AB" || got.String() != "ABC" {
		t.Fatalf("content %q deltas %q", resp.Message.Content, got.String())
	}
	if resp.Model != "mock" || mock.ID() != "mock" {
		t.Fatalf("id %s model %s", mock.ID(), resp.Model)
	}
}

func TestMockGatewayTimeoutMatchesLiveClassification(t *testing.T) {
	mock := NewMock(MustNewScript(Step{Fault: &Fault{Kind: FaultHTTP5xx, Status: 504}}))
	_, err := mock.Chat(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "q"}}})
	if !errors.Is(err, ErrTimeout) || !Retryable(err) {
		t.Fatalf("504: %v", err)
	}
	var ee *Error
	if !errors.As(err, &ee) || ee.Status != 504 {
		t.Fatalf("%v", err)
	}
}

func TestInvalidScripts(t *testing.T) {
	cases := []string{
		`{}`,
		`{"steps": null}`,
		`{"steps": [null]}`,
		`{"steps": [{}]}`,
		`{"steps": [{"content": "x", "fault": {"kind": "timeout"}}]}`,
		`{"steps": [{"fault": {"kind": "nope"}}]}`,
		`{"steps": [{"fault": {"kind": "http_5xx", "status": 404}}]}`,
		`{"steps": [{"fault": {"kind": "timeout", "delay_ms": -1}}]}`,
		`{"steps": [{"content": "x", "tool_calls": [{"id": "", "name": "t", "arguments": ""}]}]}`,
		`{"steps": [{"when": {"role": "nope"}, "content": "x"}]}`,
		`{"steps": [{"content": "x"}], "extra": true}`,
		`{"steps": [{"content": "x", "nope": 1}]}`,
		`not json`,
	}
	for _, raw := range cases {
		if _, err := ParseScript([]byte(raw)); !errors.Is(err, ErrInvalidScript) {
			t.Fatalf("script %s: %v", raw, err)
		}
	}
	if _, err := NewMock(nil).Chat(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "q"}}}); !errors.Is(err, ErrInvalidScript) {
		t.Fatal(err)
	}
	loaded, err := LoadScript(strings.NewReader(sampleScript))
	if err != nil || loaded == nil {
		t.Fatal(err)
	}
	clone := loaded.Clone()
	if _, err := NewMock(loaded).Chat(context.Background(), hi()); err != nil {
		t.Fatal(err)
	}
	resp, err := NewMock(clone).Chat(context.Background(), hi())
	if err != nil || resp.ID != "mock-1" {
		t.Fatalf("clone shared cursor: %#v %v", resp, err)
	}
}

func TestScriptLoadRejectsOversize(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString(`{"steps":[{"content":"`)
	buf.WriteString(strings.Repeat("a", maxScriptBytes))
	buf.WriteString(`"}]}`)
	if _, err := LoadScript(&buf); !errors.Is(err, ErrInvalidScript) {
		t.Fatalf("oversize: %v", err)
	}
}

func collectScript(t *testing.T, raw []byte) []traceItem {
	t.Helper()
	script, err := ParseScript(raw)
	if err != nil {
		t.Fatal(err)
	}
	return collect(t, NewMock(script))
}

func collect(t *testing.T, p Provider) []traceItem {
	t.Helper()
	var out []traceItem
	for i := 0; i < 20; i++ {
		var deltas []Delta
		resp, err := p.Chat(context.Background(), Request{
			Stream:   true,
			Messages: []Message{{Role: RoleUser, Content: "hi"}},
			OnDelta: func(d Delta) error {
				deltas = append(deltas, d)
				return nil
			},
		})
		item := traceItem{Resp: resp, Deltas: deltas}
		if err != nil {
			var ee *Error
			if errors.As(err, &ee) {
				item.ErrCode = ee.Code
				item.Retryable = ee.Retryable
			}
			item.ErrText = err.Error()
			out = append(out, item)
			if errors.Is(err, ErrScriptExhausted) {
				return out
			}
			continue
		}
		out = append(out, item)
	}
	t.Fatal("script did not exhaust")
	return nil
}

func assertFault(t *testing.T, item traceItem, code string, retryable bool) {
	t.Helper()
	if item.ErrCode != code {
		t.Fatalf("code %s want %s (%s)", item.ErrCode, code, item.ErrText)
	}
	if item.Resp.ID != "" {
		t.Fatalf("fault returned a response %#v", item.Resp)
	}
	if item.Retryable != retryable {
		t.Fatalf("retryable %v want %v for %s", item.Retryable, retryable, item.ErrText)
	}
}

func deltaText(deltas []Delta) string {
	var b strings.Builder
	for _, d := range deltas {
		b.WriteString(d.Content)
	}
	return b.String()
}

func hi() Request {
	return Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}}
}
