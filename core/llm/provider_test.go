package llm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loveRyujin/lantern/core/errcode"
)

func TestErrorClassification(t *testing.T) {
	err := &Error{Code: CodeTimeout, Retryable: true, Message: "注入超时", Err: context.DeadlineExceeded}
	if !errors.Is(err, ErrTimeout) {
		t.Fatal("errors.Is timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("errors.Is deadline")
	}
	if errors.Is(err, ErrLLM5xx) {
		t.Fatal("timeout matched 5xx")
	}
	if !Retryable(err) {
		t.Fatal("timeout should be retryable")
	}
	plain := errors.New("nope")
	if Retryable(plain) {
		t.Fatal("plain error is not retryable")
	}
	if got := (&Error{Code: CodeLLM5xx, Status: 503, Message: "unavailable"}).Error(); got != "llm: llm_5xx: HTTP 503: unavailable" {
		t.Fatalf("Error() = %q", got)
	}
}

func TestRedactKeepsShortSecretsAndScrubsLongOnes(t *testing.T) {
	if got := redact("token ab", "ab"); got != "token ab" {
		t.Fatalf("short secret was rewritten: %q", got)
	}
	secret := "super-secret-api-key"
	err := scrubError(&Error{Code: CodeLLM5xx, Status: 500, Message: "boom " + secret, Err: errors.New("dial " + secret)}, secret)
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("message leaked secret: %v", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("message = %v", err)
	}
	if strings.Contains(errors.Unwrap(err).Error(), secret) {
		t.Fatalf("unwrap leaked secret: %v", errors.Unwrap(err))
	}
	truncated := scrubError(streamTruncated("cut "+secret), secret)
	if !errors.Is(truncated, ErrStreamTruncated) || !Retryable(truncated) {
		t.Fatal(truncated)
	}
	code, ok := errcode.CodeOf(truncated)
	if !ok || code != errcode.StreamTruncated {
		t.Fatalf("code %q %v", code, ok)
	}
	if strings.Contains(truncated.Error(), secret) {
		t.Fatalf("stable error leaked secret: %v", truncated)
	}
}

func TestMapAndEnvSecrets(t *testing.T) {
	ctx := context.Background()
	if _, err := (MapSecrets{"grok_api_key": "k"}).Get(ctx, "missing"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("map missing: %v", err)
	}
	got, err := (MapSecrets{"grok_api_key": "k"}).Get(ctx, DefaultSecretName)
	if err != nil || got != "k" {
		t.Fatalf("map get %q %v", got, err)
	}
	env := EnvSecrets{Lookup: func(name string) (string, bool) {
		if name == "XAI_API_KEY" {
			return "from-env", true
		}
		return "", false
	}}
	got, err = env.Get(ctx, EnvAPIKey)
	if err != nil || got != "from-env" {
		t.Fatalf("env get %q %v", got, err)
	}
	if _, err := env.Get(ctx, "NOPE"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("env missing: %v", err)
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := env.Get(cancelCtx, EnvAPIKey); !errors.Is(err, ErrCanceled) {
		t.Fatalf("canceled get: %v", err)
	}
}

func TestValidateRequest(t *testing.T) {
	ok := Request{
		Messages: []Message{
			{Role: RoleSystem, Content: "sys"},
			{Role: RoleUser, Content: "hi"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "run_sql", Arguments: "not-json"}}},
			{Role: RoleTool, ToolCallID: "c1", Content: "1"},
		},
		Tools: []Tool{{Name: "run_sql", Parameters: []byte(`{"type":"object"}`)}},
	}
	if err := validateRequest(ok); err != nil {
		t.Fatal(err)
	}
	cases := []Request{
		{},
		{Messages: []Message{{Role: "nope", Content: "x"}}},
		{Messages: []Message{{Role: RoleUser, ToolCalls: []ToolCall{{ID: "c", Name: "t"}}}}},
		{Messages: []Message{{Role: RoleTool, Content: "x"}}},
		{Messages: []Message{{Role: RoleUser, Content: "x"}}, Tools: []Tool{{Name: "t", Parameters: []byte(`{`)}}},
		{Messages: []Message{{Role: RoleUser, Content: "x"}}, Tools: []Tool{{Name: "t"}, {Name: "t"}}},
		{Messages: []Message{{Role: RoleUser, Content: "x"}}, MaxTokens: -1},
	}
	for i, req := range cases {
		if err := validateRequest(req); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("case %d: %v", i, err)
		}
	}
}

func TestClassifyContext(t *testing.T) {
	if err := classifyContext(context.DeadlineExceeded); !errors.Is(err, ErrTimeout) || !Retryable(err) {
		t.Fatalf("deadline: %v", err)
	}
	if err := classifyContext(context.Canceled); !errors.Is(err, ErrCanceled) || Retryable(err) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestPackageDoesNotImportMygo(t *testing.T) {
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range matches {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "github.com/egoist/mygo") {
			t.Fatalf("%s imports mygo", name)
		}
	}
}

func TestInjectedTimeoutIsNotAContextDeadline(t *testing.T) {
	mock := NewMock(MustNewScript(Inject(FaultTimeout)))
	_, err := mock.Chat(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if !errors.Is(err, ErrTimeout) || !Retryable(err) {
		t.Fatalf("timeout: %v", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("injected timeout should not pretend the context deadline fired")
	}
	if !strings.Contains(err.Error(), "注入超时") {
		t.Fatal(err)
	}
}
