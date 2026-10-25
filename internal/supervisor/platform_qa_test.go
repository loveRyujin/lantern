//go:build !darwin

package supervisor

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestSupervisorQAUnsupportedPlatformDoesNotExecuteOrFallback(t *testing.T) {
	base := t.TempDir()
	root, evidence := filepath.Join(base, "unused-work"), filepath.Join(base, "unused-evidence")
	r, err := New(Config{TempRoot: root, EvidenceRoot: evidence}).Run(t.Context(), Request{Kind: GoLifecycle, Mode: Normal})
	if !errors.Is(err, ErrUnsupported) || r.State != Failed || r.Error == "" {
		t.Fatalf("unsupported API did not explicitly reject execution: state=%q error=%v detail=%q", r.State, err, r.Error)
	}
	qaSupervisorUntouched(t, r, root, evidence)
}
