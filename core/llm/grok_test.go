package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/errcode"
)

func TestChatCompletionsURL(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "https://api.x.ai/v1", want: "https://api.x.ai/v1/chat/completions"},
		{in: "https://api.x.ai/v1/", want: "https://api.x.ai/v1/chat/completions"},
		{in: "https://api.x.ai/v1?q=1#f", want: "https://api.x.ai/v1/chat/completions"},
		{in: "http://127.0.0.1:9", want: "http://127.0.0.1:9/chat/completions"},
		{in: "http://localhost:9/v1", want: "http://localhost:9/v1/chat/completions"},
		{in: "http://[::1]:9", want: "http://[::1]:9/chat/completions"},
		{in: "", want: "https://api.x.ai/v1/chat/completions"},
		{in: "ftp://api.x.ai/v1", wantErr: true},
		{in: "https://user:pass@api.x.ai/v1", wantErr: true},
		{in: "http://example.com/v1", wantErr: true},
		{in: "http://192.168.1.1:9", wantErr: true},
		{in: "not a url", wantErr: true},
	}
	for _, tc := range cases {
		got, err := chatCompletionsURL(tc.in)
		if tc.wantErr {
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("%q err %v", tc.in, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Fatalf("%q got %q %v", tc.in, got, err)
		}
	}
}

func TestGrokIDAndModelDefault(t *testing.T) {
	if got := NewGrok().ID(); got != "grok:"+DefaultModel {
		t.Fatal(got)
	}
	if got := NewGrok(WithModel("  grok-custom  ")).ID(); got != "grok:grok-custom" {
		t.Fatal(got)
	}
	if got := NewGrok(WithModel("")).ID(); got != "grok:"+DefaultModel {
		t.Fatal(got)
	}
	if DefaultModel != "grok-4.6" || DefaultBaseURL != "https://api.x.ai/v1" {
		t.Fatalf("defaults %s %s", DefaultModel, DefaultBaseURL)
	}
}

func TestGrokKeyResolution(t *testing.T) {
	const (
		explicit = "explicit-api-key-0123456789"
		fromXAI  = "xai-env-api-key-0123456789"
		fromGrok = "grok-env-api-key-0123456789"
		fromSec  = "secret-store-key-0123456789"
	)
	t.Run("explicit beats env and secrets", func(t *testing.T) {
		auth := recordAuth(t, WithAPIKey("Bearer "+explicit), WithSecrets(MapSecrets{DefaultSecretName: fromSec}), WithEnvLookup(func(k string) (string, bool) {
			return fromXAI, true
		}))
		if auth() != "Bearer "+explicit {
			t.Fatal(auth())
		}
	})
	t.Run("xai env beats alias and secrets", func(t *testing.T) {
		secretsCalled := false
		auth := recordAuth(t, WithSecrets(secretFunc(func(context.Context, string) (string, error) {
			secretsCalled = true
			return fromSec, nil
		})), WithEnvLookup(func(k string) (string, bool) {
			switch k {
			case EnvAPIKey:
				return "  " + fromXAI + " ", true
			case EnvAPIKeyAlias:
				return fromGrok, true
			default:
				return "", false
			}
		}))
		if auth() != "Bearer "+fromXAI {
			t.Fatal(auth())
		}
		if secretsCalled {
			t.Fatal("secrets consulted")
		}
	})
	t.Run("alias then secrets", func(t *testing.T) {
		var gotName string
		auth := recordAuth(t, WithSecretName("lantern-grok"), WithSecrets(secretFunc(func(_ context.Context, name string) (string, error) {
			gotName = name
			return fromSec, nil
		})), WithEnvLookup(func(k string) (string, bool) {
			if k == EnvAPIKeyAlias {
				return fromGrok, true
			}
			return "", false
		}))
		if auth() != "Bearer "+fromGrok {
			t.Fatal(auth())
		}
		if gotName != "" {
			t.Fatal("secrets consulted despite alias")
		}
	})
	t.Run("secrets name", func(t *testing.T) {
		var gotName string
		auth := recordAuth(t, WithSecretName("lantern-grok"), WithSecrets(secretFunc(func(_ context.Context, name string) (string, error) {
			gotName = name
			if name != "lantern-grok" {
				return "", ErrSecretNotFound
			}
			return fromSec, nil
		})))
		if auth() != "Bearer "+fromSec || gotName != "lantern-grok" {
			t.Fatalf("auth %s name %s", auth(), gotName)
		}
	})
	t.Run("missing key does not dial", func(t *testing.T) {
		called := false
		g := NewGrok(WithEnvLookup(func(string) (string, bool) { return "", false }), WithHTTPClient(&http.Client{
			Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				called = true
				return nil, errors.New("dialed")
			}),
		}))
		_, err := g.Chat(context.Background(), hi())
		if called || !errors.Is(err, ErrMissingAPIKey) {
			t.Fatalf("called %v err %v", called, err)
		}
	})
	t.Run("secret store failure", func(t *testing.T) {
		g := NewGrok(WithSecrets(secretFunc(func(context.Context, string) (string, error) {
			return "", errors.New("keychain locked")
		})))
		_, err := g.Chat(context.Background(), hi())
		var ee *Error
		if !errors.As(err, &ee) || ee.Code != CodeSecret {
			t.Fatal(err)
		}
	})
	t.Run("newline key rejected", func(t *testing.T) {
		g := NewGrok(WithAPIKey("abcd\nkey-0123456789"))
		_, err := g.Chat(context.Background(), hi())
		if !errors.Is(err, ErrInvalidRequest) {
			t.Fatal(err)
		}
	})
}

func TestGrokRequestShape(t *testing.T) {
	temp := 0.0
	var gotBody []byte
	var gotHeader http.Header
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		gotHeader = r.Header.Clone()
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"c1","model":"grok-4.6",
			"choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"run_sql","arguments":"{\"query\":\"SELECT 1\"}"}}]},"finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":8,"completion_tokens":4,"total_tokens":12}
		}`)
	}))
	t.Cleanup(srv.Close)
	key := "request-shape-api-key-0123456789"
	g := NewGrok(WithBaseURL(srv.URL), WithAPIKey(key), WithModel("grok-custom"), WithEnvLookup(func(string) (string, bool) { return "", false }))
	resp, err := g.Chat(context.Background(), Request{
		Temperature: &temp,
		MaxTokens:   16,
		ToolChoice:  "run_sql",
		Messages: []Message{
			{Role: RoleSystem, Content: "a < b"},
			{Role: RoleUser, Content: "问口径"},
			{Role: RoleAssistant, ReasoningContent: "think", ToolCalls: []ToolCall{{ID: "call_1", Name: "run_sql", Arguments: `{"query":"SELECT 1"}`}}},
			{Role: RoleTool, ToolCallID: "call_1", Content: "1"},
		},
		Tools: []Tool{{Name: "run_sql", Description: "只读 SQL", Parameters: []byte(`{"type":"object","properties":{"query":{"type":"string"}}}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].Arguments != `{"query":"SELECT 1"}` || resp.FinishReason != FinishToolCalls {
		t.Fatalf("%#v", resp)
	}
	if resp.Usage.PromptTokens != 8 || resp.Message.Content != "" {
		t.Fatalf("%#v", resp)
	}
	if strings.Contains(gotURL, key) || bytesContains(gotBody, key) {
		t.Fatal("api key leaked into url or body")
	}
	if gotHeader.Get("Authorization") != "Bearer "+key {
		t.Fatal(gotHeader.Get("Authorization"))
	}
	if strings.Contains(string(gotBody), `\u003c`) {
		t.Fatalf("html escaped: %s", gotBody)
	}
	var wire map[string]any
	if err := json.Unmarshal(gotBody, &wire); err != nil {
		t.Fatalf("%v %s", err, gotBody)
	}
	if wire["model"] != "grok-custom" || wire["temperature"] != 0.0 || wire["max_completion_tokens"] != 16.0 {
		t.Fatalf("scalars %#v", wire)
	}
	if _, ok := wire["stream"]; ok {
		t.Fatalf("non-stream request included stream: %s", gotBody)
	}
	choice := wire["tool_choice"].(map[string]any)
	fn := choice["function"].(map[string]any)
	if choice["type"] != "function" || fn["name"] != "run_sql" {
		t.Fatalf("tool_choice %#v", choice)
	}
	msgs := wire["messages"].([]any)
	assistant := msgs[2].(map[string]any)
	if assistant["content"] != nil {
		t.Fatalf("assistant content %#v", assistant["content"])
	}
	if assistant["reasoning_content"] != "think" {
		t.Fatalf("reasoning %#v", assistant["reasoning_content"])
	}
	toolMsg := msgs[3].(map[string]any)
	if toolMsg["tool_call_id"] != "call_1" || toolMsg["content"] != "1" {
		t.Fatalf("tool message %#v", toolMsg)
	}
	tools := wire["tools"].([]any)
	params := tools[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
	if params["type"] != "object" {
		t.Fatalf("params %#v", params)
	}
}

func TestGrokSSEToolCalls(t *testing.T) {
	body := sseBody(t, []any{
		map[string]any{"id": "c1", "model": "grok-4.6", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}}}},
		map[string]any{"id": "c1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": "先想"}}}},
		map[string]any{"id": "c1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "答"}}}},
		map[string]any{"id": "c1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "案"}}}},
		map[string]any{"id": "c1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call_1", "type": "function", "function": map[string]any{"name": "run_sql", "arguments": ""}}}}}}},
		map[string]any{"id": "c1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"name": "run_sql", "arguments": `{"q":`}}}}}}},
		map[string]any{"id": "c1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": "1}"}}}}}}},
		map[string]any{"id": "c1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 1, "id": "call_2", "type": "function", "function": map[string]any{"name": "ping", "arguments": "{}"}}}}}}},
		map[string]any{"id": "c1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}}, "usage": map[string]any{"prompt_tokens": 4, "completion_tokens": 6, "total_tokens": 10, "completion_tokens_details": map[string]any{"reasoning_tokens": 3}}},
	})
	var sawStream bool
	g, _ := newFixtureGrok(t, http.StatusOK, "text/event-stream", body, func(r *http.Request, raw []byte) {
		var wire map[string]any
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatal(err)
		}
		sawStream = wire["stream"] == true
		opts := wire["stream_options"].(map[string]any)
		if opts["include_usage"] != true {
			t.Fatalf("stream_options %#v", opts)
		}
		if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			t.Fatal(r.Header.Get("Accept"))
		}
	})
	var text strings.Builder
	var frags []ToolCallFragment
	var finish string
	resp, err := g.Chat(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		OnDelta: func(d Delta) error {
			text.WriteString(d.Content)
			frags = append(frags, d.ToolCalls...)
			if d.FinishReason != "" {
				finish = d.FinishReason
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawStream {
		t.Fatal("stream flag not set")
	}
	if text.String() != "答案" || resp.Message.Content != "答案" || resp.Message.ReasoningContent != "先想" {
		t.Fatalf("text %q resp %#v", text.String(), resp.Message)
	}
	if finish != FinishToolCalls || resp.FinishReason != FinishToolCalls {
		t.Fatalf("finish %s %#v", finish, resp)
	}
	if resp.Usage.ReasoningTokens != 3 || resp.Usage.TotalTokens != 10 || resp.ID != "c1" {
		t.Fatalf("usage %#v", resp)
	}
	if len(resp.Message.ToolCalls) != 2 {
		t.Fatalf("calls %#v", resp.Message.ToolCalls)
	}
	if resp.Message.ToolCalls[0].ID != "call_1" || resp.Message.ToolCalls[0].Name != "run_sql" || resp.Message.ToolCalls[0].Arguments != `{"q":1}` {
		t.Fatalf("call0 %#v frags %#v", resp.Message.ToolCalls[0], frags)
	}
	if resp.Message.ToolCalls[1].Name != "ping" || resp.Message.ToolCalls[1].Arguments != "{}" {
		t.Fatalf("call1 %#v", resp.Message.ToolCalls[1])
	}
}

func TestGrokHTTPFailures(t *testing.T) {
	key := "super-secret-api-key-0123456789"
	t.Run("5xx redacts key", func(t *testing.T) {
		g, _ := newFixtureGrok(t, http.StatusServiceUnavailable, "application/json", `{"error":{"message":"overloaded `+key+`"}}`, nil)
		g.apiKey = key
		_, err := g.Chat(context.Background(), hi())
		if !errors.Is(err, ErrLLM5xx) || !Retryable(err) {
			t.Fatal(err)
		}
		if strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "[REDACTED]") {
			t.Fatal(err)
		}
		var ee *Error
		if !errors.As(err, &ee) || ee.Status != http.StatusServiceUnavailable {
			t.Fatalf("%#v", ee)
		}
	})
	t.Run("401", func(t *testing.T) {
		g, _ := newFixtureGrok(t, http.StatusUnauthorized, "application/json", `{"error":"bad key"}`, nil)
		_, err := g.Chat(context.Background(), hi())
		var ee *Error
		if !errors.As(err, &ee) || ee.Code != CodeUnauthorized || ee.Retryable || ee.Status != 401 {
			t.Fatal(err)
		}
	})
	t.Run("429", func(t *testing.T) {
		g, _ := newFixtureGrok(t, http.StatusTooManyRequests, "text/plain", "slow down", nil)
		_, err := g.Chat(context.Background(), hi())
		var ee *Error
		if !errors.As(err, &ee) || ee.Code != CodeRateLimited || !ee.Retryable {
			t.Fatal(err)
		}
	})
	t.Run("504", func(t *testing.T) {
		g, _ := newFixtureGrok(t, http.StatusGatewayTimeout, "text/plain", "", nil)
		_, err := g.Chat(context.Background(), hi())
		if !errors.Is(err, ErrTimeout) || !Retryable(err) {
			t.Fatal(err)
		}
	})
	t.Run("302 is not followed", func(t *testing.T) {
		g, _ := newFixtureGrok(t, http.StatusFound, "text/plain", "go away", nil)
		_, err := g.Chat(context.Background(), hi())
		var ee *Error
		if !errors.As(err, &ee) || ee.Code != CodeHTTP || ee.Status != http.StatusFound {
			t.Fatal(err)
		}
	})
	t.Run("invalid json", func(t *testing.T) {
		g, _ := newFixtureGrok(t, http.StatusOK, "application/json", "not-json", nil)
		_, err := g.Chat(context.Background(), hi())
		if !errors.Is(err, ErrInvalidJSON) || Retryable(err) {
			t.Fatal(err)
		}
	})
	t.Run("empty choices", func(t *testing.T) {
		g, _ := newFixtureGrok(t, http.StatusOK, "application/json", `{"choices":[]}`, nil)
		_, err := g.Chat(context.Background(), hi())
		if !errors.Is(err, ErrEmptyResponse) {
			t.Fatal(err)
		}
	})
	t.Run("api error json", func(t *testing.T) {
		g, _ := newFixtureGrok(t, http.StatusOK, "application/json", `{"error":{"message":"boom","type":"server_error"}}`, nil)
		_, err := g.Chat(context.Background(), hi())
		if !errors.Is(err, ErrLLM5xx) || !Retryable(err) {
			t.Fatal(err)
		}
	})
	t.Run("sse invalid json", func(t *testing.T) {
		body := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\ndata: {bad\n\n"
		g, _ := newFixtureGrok(t, http.StatusOK, "text/event-stream", body, nil)
		var saw string
		_, err := g.Chat(context.Background(), Request{
			Stream:   true,
			Messages: []Message{{Role: RoleUser, Content: "q"}},
			OnDelta: func(d Delta) error {
				saw += d.Content
				return nil
			},
		})
		if !errors.Is(err, ErrInvalidJSON) || saw != "hi" {
			t.Fatalf("saw %q err %v", saw, err)
		}
	})
	t.Run("short key still clips body", func(t *testing.T) {
		huge := strings.Repeat("x", 4000)
		g, _ := newFixtureGrok(t, http.StatusServiceUnavailable, "text/plain", huge, nil)
		g.apiKey = "short"
		_, err := g.Chat(context.Background(), hi())
		var ee *Error
		if !errors.As(err, &ee) || ee.Code != CodeLLM5xx {
			t.Fatal(err)
		}
		if n := len([]rune(ee.Message)); n > 513 {
			t.Fatalf("body was not clipped: %d runes", n)
		}
	})
	t.Run("sse done only", func(t *testing.T) {
		g, _ := newFixtureGrok(t, http.StatusOK, "text/event-stream", "data: [DONE]\n", nil)
		_, err := g.Chat(context.Background(), Request{Stream: true, Messages: []Message{{Role: RoleUser, Content: "q"}}})
		if !errors.Is(err, ErrEmptyResponse) {
			t.Fatal(err)
		}
	})
	t.Run("json fallback while streaming", func(t *testing.T) {
		g, _ := newFixtureGrok(t, http.StatusOK, "application/json", `{"id":"c9","model":"grok-4.6","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}]}`, nil)
		var got string
		resp, err := g.Chat(context.Background(), Request{
			Stream:   true,
			Messages: []Message{{Role: RoleUser, Content: "q"}},
			OnDelta: func(d Delta) error {
				got += d.Content
				return nil
			},
		})
		if err != nil || resp.Message.Content != "pong" || got != "pong" {
			t.Fatalf("resp %#v got %q err %v", resp, got, err)
		}
	})
}

func TestGrokSSEContentCutOff(t *testing.T) {
	body := sseEvents(t,
		map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "你好"}}}},
		map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "世界"}}}},
	)
	g, _ := newFixtureGrok(t, http.StatusOK, "text/event-stream", body, nil)
	var saw string
	resp, err := g.Chat(context.Background(), Request{
		Stream:   true,
		Messages: []Message{{Role: RoleUser, Content: "q"}},
		OnDelta: func(d Delta) error {
			saw += d.Content
			return nil
		},
	})
	assertStreamTruncated(t, resp, err)
	if saw != "你好世界" {
		t.Fatalf("deltas %q", saw)
	}
	if strings.Contains(resp.Message.Content, "你好") {
		t.Fatalf("truncated content returned %#v", resp)
	}
}

func TestGrokSSEToolArgumentsCutOff(t *testing.T) {
	body := sseEvents(t, map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{
		"index":    0,
		"id":       "call_1",
		"type":     "function",
		"function": map[string]any{"name": "run_sql", "arguments": `{"q":`},
	}}}}}})
	g, _ := newFixtureGrok(t, http.StatusOK, "text/event-stream", body, nil)
	var sawArgs string
	resp, err := g.Chat(context.Background(), Request{
		Stream:   true,
		Messages: []Message{{Role: RoleUser, Content: "q"}},
		OnDelta: func(d Delta) error {
			for _, frag := range d.ToolCalls {
				sawArgs += frag.Arguments
			}
			return nil
		},
	})
	assertStreamTruncated(t, resp, err)
	if sawArgs != `{"q":` {
		t.Fatalf("deltas %q", sawArgs)
	}
	if len(resp.Message.ToolCalls) != 0 {
		t.Fatalf("partial tool call returned %#v", resp.Message.ToolCalls)
	}
}

func TestGrokSSEToolCallMissingIDOrName(t *testing.T) {
	cases := []struct {
		name string
		id   string
		tool string
	}{
		{name: "missing id", id: "", tool: "run_sql"},
		{name: "missing name", id: "call_1", tool: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := sseBody(t, []any{
				map[string]any{"choices": []any{map[string]any{
					"index": 0,
					"delta": map[string]any{"tool_calls": []any{map[string]any{
						"index":    0,
						"id":       tc.id,
						"type":     "function",
						"function": map[string]any{"name": tc.tool, "arguments": "{}"},
					}}},
					"finish_reason": "tool_calls",
				}}},
			})
			g, _ := newFixtureGrok(t, http.StatusOK, "text/event-stream", body, nil)
			resp, err := g.Chat(context.Background(), Request{
				Stream:   true,
				Messages: []Message{{Role: RoleUser, Content: "q"}},
			})
			assertInvalidToolCall(t, resp, err)
		})
	}
	t.Run("json fallback while streaming", func(t *testing.T) {
		raw := `{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"","type":"function","function":{"name":"run_sql","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`
		g, _ := newFixtureGrok(t, http.StatusOK, "application/json", raw, nil)
		called := false
		resp, err := g.Chat(context.Background(), Request{
			Stream:   true,
			Messages: []Message{{Role: RoleUser, Content: "q"}},
			OnDelta: func(Delta) error {
				called = true
				return nil
			},
		})
		assertInvalidToolCall(t, resp, err)
		if called {
			t.Fatal("invalid tool call was emitted as a delta")
		}
	})
	t.Run("non-stream json", func(t *testing.T) {
		raw := `{"choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`
		g, _ := newFixtureGrok(t, http.StatusOK, "application/json", raw, nil)
		resp, err := g.Chat(context.Background(), hi())
		assertInvalidToolCall(t, resp, err)
	})
}

func TestGrokSSEFinishWithoutDone(t *testing.T) {
	body := sseEvents(t, map[string]any{"choices": []any{map[string]any{
		"index":         0,
		"delta":         map[string]any{"content": "完整"},
		"finish_reason": "stop",
	}}})
	g, _ := newFixtureGrok(t, http.StatusOK, "text/event-stream", body, nil)
	resp, err := g.Chat(context.Background(), Request{
		Stream:   true,
		Messages: []Message{{Role: RoleUser, Content: "q"}},
	})
	if err != nil || resp.Message.Content != "完整" || resp.FinishReason != FinishStop {
		t.Fatalf("resp %#v err %v", resp, err)
	}
}

func assertStreamTruncated(t *testing.T, resp Response, err error) {
	t.Helper()
	if !errors.Is(err, ErrStreamTruncated) || !Retryable(err) {
		t.Fatalf("err %v", err)
	}
	code, ok := errcode.CodeOf(err)
	if !ok || code != errcode.StreamTruncated {
		t.Fatalf("code %q ok %v err %v", code, ok, err)
	}
	if !responseIsZero(resp) {
		t.Fatalf("response %#v", resp)
	}
}

func assertInvalidToolCall(t *testing.T, resp Response, err error) {
	t.Helper()
	var ee *Error
	if !errors.As(err, &ee) || ee.Code != CodeInvalidResponse || ee.Retryable {
		t.Fatalf("err %v", err)
	}
	if errors.Is(err, ErrStreamTruncated) {
		t.Fatal("missing id or name was treated as truncation")
	}
	if !responseIsZero(resp) {
		t.Fatalf("response %#v", resp)
	}
}

func responseIsZero(resp Response) bool {
	return resp.ID == "" && resp.Model == "" && resp.FinishReason == "" && resp.Usage == (Usage{}) &&
		resp.Message.Role == "" && resp.Message.Content == "" && resp.Message.ReasoningContent == "" &&
		resp.Message.Name == "" && resp.Message.ToolCallID == "" && len(resp.Message.ToolCalls) == 0
}

func TestGrokContext(t *testing.T) {
	t.Run("already canceled", func(t *testing.T) {
		called := false
		g := NewGrok(WithAPIKey("test-api-key-0123456789"), WithHTTPClient(&http.Client{
			Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				called = true
				return nil, errors.New("dialed")
			}),
		}))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := g.Chat(ctx, hi())
		if called || !errors.Is(err, ErrCanceled) {
			t.Fatalf("called %v err %v", called, err)
		}
	})
	t.Run("already deadline", func(t *testing.T) {
		g := NewGrok(WithAPIKey("test-api-key-0123456789"), WithHTTPClient(&http.Client{
			Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("dialed")
				return nil, nil
			}),
		}))
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		_, err := g.Chat(ctx, hi())
		if !errors.Is(err, ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	})
	t.Run("in flight", func(t *testing.T) {
		g := NewGrok(WithAPIKey("test-api-key-0123456789"), WithHTTPClient(&http.Client{
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				<-r.Context().Done()
				return nil, r.Context().Err()
			}),
		}))
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		_, err := g.Chat(ctx, hi())
		if !errors.Is(err, ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	})
}

func TestGrokSmokeExcludedFromDefaultBuild(t *testing.T) {
	body, err := os.ReadFile("grok_smoke_test.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(body), "//go:build grok\n") {
		t.Fatal("smoke file must start with //go:build grok")
	}
	defaultList := goTestList(t)
	if strings.Contains(defaultList, "TestGrokLiveSmoke") || strings.Contains(defaultList, "TestGrokLiveToolCall") {
		t.Fatalf("default test list includes grok smoke:\n%s", defaultList)
	}
	tagged := goTestList(t, "-tags=grok")
	if !strings.Contains(tagged, "TestGrokLiveSmoke") || !strings.Contains(tagged, "TestGrokLiveToolCall") {
		t.Fatalf("tagged list missing smoke:\n%s", tagged)
	}
}

func goTestList(t *testing.T, extra ...string) string {
	t.Helper()
	args := append([]string{"test"}, extra...)
	args = append(args, "-list", ".")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", args...)
	env := make([]string, 0, len(os.Environ())+2)
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "GOFLAGS=") || strings.HasPrefix(e, "CGO_ENABLED=") {
			continue
		}
		env = append(env, e)
	}
	cmd.Env = append(env, "CGO_ENABLED=0", "GOFLAGS=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go test %v: %v\n%s", extra, err, out)
	}
	return string(out)
}

type secretFunc func(context.Context, string) (string, error)

func (f secretFunc) Get(ctx context.Context, name string) (string, error) { return f(ctx, name) }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func recordAuth(t *testing.T, opts ...GrokOption) func() string {
	t.Helper()
	var mu sync.Mutex
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","model":"grok-4.6","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(srv.Close)
	base := []GrokOption{WithBaseURL(srv.URL), WithEnvLookup(func(string) (string, bool) { return "", false })}
	g := NewGrok(append(base, opts...)...)
	if _, err := g.Chat(context.Background(), hi()); err != nil {
		t.Fatal(err)
	}
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return auth
	}
}

func newFixtureGrok(t *testing.T, status int, contentType, body string, inspect func(*http.Request, []byte)) (*Grok, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if inspect != nil {
			inspect(r, raw)
		}
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	g := NewGrok(
		WithBaseURL(srv.URL),
		WithAPIKey("fixture-api-key-0123456789"),
		WithEnvLookup(func(string) (string, bool) { return "", false }),
	)
	return g, srv
}

func sseEvents(t *testing.T, chunks ...any) string {
	t.Helper()
	var b strings.Builder
	for _, chunk := range chunks {
		raw, err := json.Marshal(chunk)
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString("data: ")
		b.Write(raw)
		b.WriteString("\n\n")
	}
	return b.String()
}

func sseBody(t *testing.T, chunks []any) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(": keep-alive\n\n")
	for _, chunk := range chunks {
		raw, err := json.Marshal(chunk)
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString("event: message\n")
		b.WriteString("data: ")
		b.Write(raw)
		b.WriteString("\n\n")
	}
	b.WriteString("data: [DONE]\n")
	return b.String()
}

func bytesContains(b []byte, s string) bool {
	return strings.Contains(string(b), s)
}
