//go:build grok

// 连真实 grok-4.6 的冒烟测试。CI 的 go test ./... 不带 grok 标签，不会编译本文件。
package llm

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestGrokLiveSmoke(t *testing.T) {
	key := liveKey()
	p := NewGrok()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var streamed strings.Builder
	resp, err := p.Chat(ctx, Request{
		Stream: true,
		Messages: []Message{{
			Role:    RoleUser,
			Content: "Reply with exactly the single word pong.",
		}},
		OnDelta: func(d Delta) error {
			streamed.WriteString(d.Content)
			return nil
		},
	})
	if err != nil {
		if len(key) >= 8 && strings.Contains(err.Error(), key) {
			t.Fatal("error leaked api key")
		}
		t.Fatal(err)
	}
	if strings.TrimSpace(resp.Message.Content) == "" && strings.TrimSpace(resp.Message.ReasoningContent) == "" {
		t.Fatalf("empty live response: %#v", resp.Message)
	}
	if streamed.String() != resp.Message.Content {
		t.Fatalf("stream %q != final content %q", streamed.String(), resp.Message.Content)
	}
	if resp.Model == "" || p.ID() == "" {
		t.Fatalf("model %q id %q", resp.Model, p.ID())
	}
}

func TestGrokLiveToolCall(t *testing.T) {
	key := liveKey()
	p := NewGrok()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	resp, err := p.Chat(ctx, Request{
		Stream:     true,
		ToolChoice: "ping",
		Messages:   []Message{{Role: RoleUser, Content: "Call the ping tool."}},
		Tools: []Tool{{
			Name:        "ping",
			Description: "Return pong.",
			Parameters:  []byte(`{"type":"object","properties":{}}`),
		}},
	})
	if err != nil {
		if len(key) >= 8 && strings.Contains(err.Error(), key) {
			t.Fatal("error leaked api key")
		}
		t.Fatal(err)
	}
	if len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].Name != "ping" {
		t.Fatalf("tool call %#v finish %s", resp.Message.ToolCalls, resp.FinishReason)
	}
}

func liveKey() string {
	if v := strings.TrimSpace(os.Getenv(EnvAPIKey)); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv(EnvAPIKeyAlias))
}
