package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func qaSupervisorUntouched(t *testing.T, result Result, root, evidence string) {
	t.Helper()
	if result.HostPID != 0 || result.GuardianPID != 0 || result.Task.PID != 0 || result.Worker.Entered || result.GoAuthorized || result.Registered || result.Armed || len(result.Events) != 0 || result.WorkRoot != "" || result.EvidenceDir != "" {
		t.Fatalf("request rejection reported execution or created per-call state: %+v", result)
	}
	for _, path := range []string{root, evidence} {
		if path == "" {
			continue
		}
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("rejected request touched output %q: %v", path, err)
		}
	}
}

func TestSupervisorQARejectsUnsealedRequestsBeforeSideEffects(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*Request)
	}{
		{"missing-kind", func(q *Request) { q.Kind = "" }},
		{"command-kind", func(q *Request) { q.Kind = "/bin/sh" }},
		{"kind-with-nul", func(q *Request) { q.Kind = "go-lifecycle\x00" }},
		{"missing-mode", func(q *Request) { q.Mode = "" }},
		{"source-as-mode", func(q *Request) { q.Mode = "os.Exit(0)" }},
		{"arbitrary-command-mode", func(q *Request) { q.Mode = "/usr/bin/true" }},
		{"go-is-not-a-gate", func(q *Request) { q.Gate = "GO" }},
		{"unknown-gate", func(q *Request) { q.Gate = "running" }},
		{"path-as-gate", func(q *Request) { q.Gate = "../armed" }},
		{"legacy-is-not-registered", func(q *Request) { q.Kind = "legacy-python-lifecycle" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := t.TempDir()
			root, evidence := filepath.Join(base, "unused-work"), filepath.Join(base, "unused-evidence")
			q := Request{Kind: GoLifecycle, Mode: Normal}
			test.change(&q)
			r, err := New(Config{TempRoot: root, EvidenceRoot: evidence}).Run(t.Context(), q)
			if !errors.Is(err, ErrInvalidRequest) || r.State != Failed {
				t.Fatalf("unsealed request did not fail as invalid: state=%q error=%v", r.State, err)
			}
			qaSupervisorUntouched(t, r, root, evidence)
		})
	}
}

func TestSupervisorQAInvalidRootsDoNotMutateExistingData(t *testing.T) {
	for _, which := range []string{"relative-temp", "unclean-temp", "relative-evidence", "unclean-evidence"} {
		t.Run(which, func(t *testing.T) {
			base := t.TempDir()
			marker := filepath.Join(base, "owned-existing-data")
			want := []byte("prior data must survive request rejection")
			if err := os.WriteFile(marker, want, 0600); err != nil {
				t.Fatal(err)
			}
			c := Config{TempRoot: filepath.Join(base, "unused-work"), EvidenceRoot: filepath.Join(base, "unused-evidence")}
			switch which {
			case "relative-temp":
				c.TempRoot = "relative"
			case "unclean-temp":
				c.TempRoot = base + string(filepath.Separator) + ".." + string(filepath.Separator) + filepath.Base(base)
			case "relative-evidence":
				c.EvidenceRoot = "relative"
			case "unclean-evidence":
				c.EvidenceRoot = base + string(filepath.Separator) + "."
			}
			r, err := New(c).Run(t.Context(), Request{Kind: GoLifecycle, Mode: Normal})
			if !errors.Is(err, ErrInvalidRequest) || r.State != Failed {
				t.Fatalf("invalid root was accepted: state=%q error=%v", r.State, err)
			}
			got, err := os.ReadFile(marker)
			if err != nil || string(got) != string(want) {
				t.Fatalf("invalid config changed prior data: bytes=%q error=%v", got, err)
			}
			entries, err := os.ReadDir(base)
			if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(marker) {
				t.Fatalf("invalid config created per-call paths: %v error=%v", entries, err)
			}
		})
	}
}

func TestSupervisorQAPreCancelledAndNilContextsDoNotStartActors(t *testing.T) {
	for _, name := range []string{"nil", "cancelled", "deadline-exceeded"} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			root, evidence := filepath.Join(base, "unused-work"), filepath.Join(base, "unused-evidence")
			var ctx context.Context
			var expected error = ErrInvalidRequest
			state := Failed
			switch name {
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(t.Context())
				cancel()
				expected, state = context.Canceled, Cancelled
			case "deadline-exceeded":
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
				defer cancel()
				expected, state = context.DeadlineExceeded, Cancelled
			}
			r, err := New(Config{TempRoot: root, EvidenceRoot: evidence}).Run(ctx, Request{Kind: GoLifecycle, Mode: Normal})
			if !errors.Is(err, expected) || r.State != state {
				t.Fatalf("early context was misclassified: state=%q error=%v want=%v", r.State, err, expected)
			}
			qaSupervisorUntouched(t, r, root, evidence)
		})
	}
}

func TestSupervisorQAConstructionSnapshotsConfigWithoutFilesystemWork(t *testing.T) {
	base := t.TempDir()
	c := Config{TempRoot: filepath.Join(base, "unused-work"), EvidenceRoot: filepath.Join(base, "unused-evidence")}
	want := c
	s := New(c)
	if s == nil {
		t.Fatal("constructor returned a nil supervisor")
	}
	c.TempRoot = "/mutated/root"
	c.EvidenceRoot = "/mutated/evidence"
	if s.config != want {
		t.Fatal("caller mutation changed an existing supervisor's sealed configuration")
	}
	entries, err := os.ReadDir(base)
	if err != nil || len(entries) != 0 {
		t.Fatalf("constructor performed filesystem work: %v %v", entries, err)
	}
}
