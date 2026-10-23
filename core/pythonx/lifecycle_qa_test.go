//go:build linux && amd64

package pythonx

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/pyrt"
	"github.com/loveRyujin/lantern/core/sandbox"
	"github.com/loveRyujin/lantern/core/tools"
	"github.com/loveRyujin/lantern/core/workspace"
)

type lifecycleQARunner func(context.Context, sandbox.Spec) (sandbox.Result, error)

func (f lifecycleQARunner) Run(ctx context.Context, spec sandbox.Spec) (sandbox.Result, error) {
	return f(ctx, spec)
}

func lifecycleQASession(t *testing.T) (*Session, tools.Call) {
	t.Helper()
	ws, err := workspace.Create(t.Context(), t.TempDir(), "lifecycle-qa")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	s, err := New(t.Context(), Config{Workspace: ws, RunID: "qa"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, tools.Call{ID: "python", Name: "run_python", WorkspaceID: ws.ID, RunID: "qa", Args: json.RawMessage(`{"code":"pass"}`)}
}

// Controlled runners prove lifecycle ordering without timing a Python process.
// Separate external tests exercise the same production API with real prepared
// Python and Sandbox; these unit checks do not stand in for real execution.
func TestPythonQACloseWaitsForCleanupAndLease(t *testing.T) {
	s, call := lifecycleQASession(t)
	entered := make(chan struct{})
	canceled := make(chan struct{})
	finish := make(chan struct{})
	released := make(chan struct{})
	s.acquire = func(context.Context, pyrt.Runtime) (func(), error) { return func() { close(released) }, nil }
	s.runner = lifecycleQARunner(func(ctx context.Context, _ sandbox.Spec) (sandbox.Result, error) {
		close(entered)
		<-ctx.Done()
		close(canceled)
		<-finish
		return sandbox.Result{}, sandbox.Timeout("controlled cleanup finished")
	})
	runDone := make(chan error, 1)
	go func() { _, err := s.PythonTool().Execute(t.Context(), call); runDone <- err }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("controlled runner not entered")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- s.Close() }()
	var finishOnce sync.Once
	finishCleanup := func() { finishOnce.Do(func() { close(finish) }) }
	defer finishCleanup()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not cancel active execution")
	}
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned before runner cleanup/lease release: %v", err)
	case <-released:
		t.Fatal("runtime lease released before runner cleanup")
	case <-time.After(100 * time.Millisecond):
	}
	finishCleanup()
	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatal("runtime lease was not released after cleanup")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not finish after cleanup and lease release")
	}
	if code, ok := tools.CodeOf(<-runDone); !ok || code != "timeout" {
		t.Fatalf("Close canceled result has wrong code: %s", code)
	}
}

func TestPythonQARejectedPermissionsNeverAcquireOrRun(t *testing.T) {
	s, call := lifecycleQASession(t)
	var acquires, runs atomic.Int32
	s.acquire = func(context.Context, pyrt.Runtime) (func(), error) { acquires.Add(1); return func() {}, nil }
	s.runner = lifecycleQARunner(func(context.Context, sandbox.Spec) (sandbox.Result, error) { runs.Add(1); return sandbox.Result{}, nil })
	for _, raw := range []string{`{"code":"pass","network":true}`, `{"code":"pass","inputs":{"x":"payload"}}`, `{"code":"pass","extra_read_only":["/"]}`, `{"code":"pass","runtime":{"python":"/bin/sh"}}`, `{"code":"pass","limits":{"memory":1}}`} {
		call.Args = json.RawMessage(raw)
		result, err := s.PythonTool().Execute(t.Context(), call)
		if code, ok := tools.CodeOf(err); !ok || code != tools.CodeArgumentsInvalid || result.Content != "" {
			t.Fatalf("permission expansion was not rejected: %s result=%+v error=%v", raw, result, err)
		}
	}
	if acquires.Load() != 0 || runs.Load() != 0 {
		t.Fatalf("rejected request crossed execution boundary: acquires=%d runs=%d", acquires.Load(), runs.Load())
	}
}

func TestPythonQABudgetFailurePublishesNoPartialBatch(t *testing.T) {
	s, call := lifecycleQASession(t)
	s.artifactBytes = MaxArtifactBytes - 3
	s.artifacts["existing"] = sandbox.Artifact{Name: "existing.json", Kind: sandbox.KindJSON, Data: []byte("0")}
	s.acquire = func(context.Context, pyrt.Runtime) (func(), error) { return func() {}, nil }
	s.runner = lifecycleQARunner(func(context.Context, sandbox.Spec) (sandbox.Result, error) {
		return sandbox.Result{Artifacts: []sandbox.Artifact{{Name: "a.json", Kind: sandbox.KindJSON, Data: []byte("{}")}, {Name: "b.json", Kind: sandbox.KindJSON, Data: []byte("{}")}}}, nil
	})
	result, err := s.PythonTool().Execute(t.Context(), call)
	if code, ok := tools.CodeOf(err); !ok || code != CodeBudget || result.Content != "" {
		t.Fatalf("expected whole-batch rejection: result=%+v error=%v", result, err)
	}
	if s.artifactBytes != MaxArtifactBytes-3 || len(s.artifacts) != 1 || string(s.artifacts["existing"].Data) != "0" {
		t.Fatalf("failed batch changed store: bytes=%d entries=%d", s.artifactBytes, len(s.artifacts))
	}
}
