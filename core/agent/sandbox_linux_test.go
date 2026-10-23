//go:build linux && amd64

package agent

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/llm"
	"github.com/loveRyujin/lantern/core/sandbox"
	"github.com/loveRyujin/lantern/core/tools"
)

// This adapter exercises the real process boundary through the Agent loop.
// The production run_python tool and its policy belong to #43.
func TestLoopRealSandboxObservations(t *testing.T) {
	runner := sandbox.New(sandbox.Config{TempRoot: t.TempDir()})
	probe, err := runner.Run(t.Context(), sandbox.Spec{Code: "print('ready')"})
	if errors.Is(err, sandbox.ErrSandboxUnavailable) || errors.Is(err, sandbox.ErrRuntimeUnavailable) {
		if os.Getenv("SANDBOX_REQUIRE_REAL") == "1" {
			t.Fatalf("real sandbox required: %v", err)
		}
		t.Skipf("real sandbox unavailable: %v", err)
	}
	if err != nil || strings.TrimSpace(probe.Stdout) != "ready" {
		t.Fatalf("probe=%+v error=%v", probe, err)
	}
	t.Run("real stdout reaches model", func(t *testing.T) {
		h := newHarness(t, echoSpec(), goodStep("sandbox", "x"), llm.Say("done"))
		h.loop.Config.StepTimeout = 20 * time.Second
		h.loop.Config.TotalTimeout = 30 * time.Second
		h.echo.fn = func(ctx context.Context, _ tools.Call) (tools.Result, error) {
			res, err := runner.Run(ctx, sandbox.Spec{Code: "print('isolated-python-observation')"})
			return tools.Result{Content: res.Stdout}, err
		}
		out, err := h.run()
		if err != nil || out.State != StateCompleted || h.echo.n() != 1 {
			t.Fatalf("outcome=%+v error=%v calls=%d", out, err, h.echo.n())
		}
		if feedback := requestText(h.provider.requests()[1]); !strings.Contains(feedback, "isolated-python-observation") {
			t.Fatalf("missing real observation: %s", feedback)
		}
	})
	t.Run("real OOM preserves parameter failure count", func(t *testing.T) {
		h := newHarness(t, echoSpec(), badStep("b1"), badStep("b2"), goodStep("oom", "x"), badStep("b3"), llm.Say("unreachable"))
		h.loop.Config.StepTimeout = 20 * time.Second
		h.loop.Config.TotalTimeout = 30 * time.Second
		h.echo.fn = func(ctx context.Context, _ tools.Call) (tools.Result, error) {
			res, err := runner.Run(ctx, sandbox.Spec{Code: "import time\nx=bytearray(128<<20)\ntime.sleep(2)", Limits: sandbox.Limits{Memory: 32 << 20, Wall: 5 * time.Second}})
			if !errors.Is(err, sandbox.ErrOOM) {
				t.Errorf("want real sampled OOM: result=%+v error=%v", res, err)
			}
			return tools.Result{Content: res.Stdout}, err
		}
		out, err := h.run()
		if out.ModelCalls != 4 || h.provider.n() != 4 || h.echo.n() != 1 || out.State != StateFailed || errorCode(err) != tools.CodeArgumentsInvalid {
			t.Fatalf("outcome=%+v error=%v calls=%d", out, err, h.echo.n())
		}
		if feedback := requestText(h.provider.requests()[3]); !strings.Contains(feedback, "oom") {
			t.Fatalf("real OOM not returned to model: %s", feedback)
		}
	})
}
