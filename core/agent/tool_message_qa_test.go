package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/loveRyujin/lantern/core/llm"
	"github.com/loveRyujin/lantern/core/tools"
)

// This fixture deliberately puts the artifact identity after the old 500-rune
// boundary. The large integer and whitespace also detect lossy re-encoding.
func qaManifestFeedback() string {
	return " \n" + `{"stdout":"` + strings.Repeat("图表\\n", 160) + `","stderr":"","stdout_truncated":false,"stderr_truncated":false,"usage":{"PeakRSS":9007199254740993},"limits":{"Memory":536870912},"artifacts":[{"id":"opaque-chart-identity","name":"chart.png","kind":"png","bytes":19896}]}` + "\n "
}

func TestToolMessageQAPreservesStructuredResult(t *testing.T) {
	manifest := qaManifestFeedback()
	if !json.Valid([]byte(manifest)) || utf8.RuneCountInString(manifest) <= 500 {
		t.Fatal("invalid regression fixture")
	}
	for name, content := range map[string]string{
		"manifest": manifest,
		"array":    `[9007199254740993,"` + strings.Repeat("x", 600) + `"]`,
		"scalar":   `"` + strings.Repeat("x", 600) + `"`,
	} {
		t.Run(name, func(t *testing.T) {
			msg := toolMessage("chart-call", content)
			if msg.Role != llm.RoleTool || msg.ToolCallID != "chart-call" || msg.Content != content {
				t.Fatalf("tool identity or complete JSON changed: role=%s call=%s input=%d output=%d", msg.Role, msg.ToolCallID, len(content), len(msg.Content))
			}
		})
	}
}

func TestToolMessageQAJSONByteBudget(t *testing.T) {
	const budget = 64 << 10 // Frozen model-feedback contract, independent of production's constant.
	exact := `{"padding":"` + strings.Repeat("x", budget-len(`{"padding":""}`)) + `"}`
	if len(exact) != budget || !json.Valid([]byte(exact)) {
		t.Fatal("invalid boundary fixture")
	}
	if got := toolMessage("exact", exact).Content; got != exact {
		t.Fatalf("exact budget JSON lost: input=%d output=%d", len(exact), len(got))
	}
	for name, content := range map[string]string{
		"one-byte-over": exact + " ",
		"unicode":       `{"stdout":"` + strings.Repeat("图", budget/3) + `","artifacts":[{"id":"must-not-survive-as-a-partial-manifest"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if len(content) <= budget || !json.Valid([]byte(content)) {
				t.Fatal("invalid over-budget fixture")
			}
			if name == "unicode" && utf8.RuneCountInString(content) >= budget {
				t.Fatal("Unicode fixture must distinguish byte and rune budgets")
			}
			msg := toolMessage("oversized-call", content)
			if msg.Role != llm.RoleTool || msg.ToolCallID != "oversized-call" {
				t.Fatal("over-budget feedback lost tool identity")
			}
			qaAssertOversizedFeedback(t, msg.Content, len(content))
		})
	}
}

func qaAssertOversizedFeedback(t *testing.T, content string, originalBytes int) {
	t.Helper()
	if len(content) >= 500 || !utf8.ValidString(content) || !json.Valid([]byte(content)) {
		t.Fatalf("oversized result did not become small valid JSON: bytes=%d", len(content))
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 4 {
		t.Fatalf("feedback must not masquerade as a partial artifact/result: %s", content)
	}
	var got struct {
		Error         string `json:"error"`
		Truncated     bool   `json:"truncated"`
		OriginalBytes int    `json:"original_bytes"`
		LimitBytes    int    `json:"limit_bytes"`
	}
	if err := json.Unmarshal([]byte(content), &got); err != nil {
		t.Fatal(err)
	}
	if got.Error != "tool_result_too_large" || !got.Truncated || got.OriginalBytes != originalBytes || got.LimitBytes != 64<<10 {
		t.Fatalf("missing explicit feedback budget: %s", content)
	}
}

func TestToolMessageQANonJSONKeepsLegacyTextLimit(t *testing.T) {
	for name, content := range map[string]string{
		"exact":           strings.Repeat("x", 500),
		"ascii":           strings.Repeat("x", 501),
		"unicode":         strings.Repeat("图", 501),
		"incomplete-json": `{"stdout":"` + strings.Repeat("x", 600),
		"invalid-utf8":    `{"stdout":"` + string([]byte{0xff}) + strings.Repeat("x", 600) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			want := []rune(content)
			if len(want) > 500 {
				want = want[:500]
			}
			if got := toolMessage("text", content).Content; got != string(want) {
				t.Fatalf("legacy text clipping changed: input=%d output=%d", len(content), len(got))
			}
		})
	}
}

// Exercise the actual Loop->next-provider-request boundary with a scripted
// tool. The separately gated production Python chart test covers real execution.
func TestToolMessageQALoopForwardsStructuredResult(t *testing.T) {
	for name, content := range map[string]string{
		"manifest":  qaManifestFeedback(),
		"oversized": `{"stdout":"` + strings.Repeat("x", 64<<10) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, echoSpec(), goodStep("structured-call", "x"), llm.Say("done"))
			h.echo.fn = func(context.Context, tools.Call) (tools.Result, error) {
				return tools.Result{Content: content, Summary: "execution succeeded"}, nil
			}
			out, err := h.run()
			if err != nil || out.State != StateCompleted || out.ModelCalls != 2 || h.echo.n() != 1 {
				t.Fatalf("feedback size changed actual execution: outcome=%+v error=%v calls=%d", out, err, h.echo.n())
			}
			var observations []llm.Message
			for _, message := range h.provider.requests()[1].Messages {
				if message.Role == llm.RoleTool {
					observations = append(observations, message)
				}
			}
			if len(observations) != 1 || observations[0].ToolCallID != "structured-call" {
				t.Fatalf("next request lost tool-call association: %+v", observations)
			}
			if name == "manifest" {
				if observations[0].Content != content {
					t.Fatal("next model request did not receive the complete manifest")
				}
			} else {
				qaAssertOversizedFeedback(t, observations[0].Content, len(content))
			}
			for _, event := range h.log.Events() {
				if event.Status == "failed" || strings.Contains(event.Summary, "opaque-chart-identity") {
					t.Fatalf("feedback budget changed execution status or leaked observation into log: %+v", event)
				}
			}
		})
	}
}
