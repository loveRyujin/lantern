//go:build !darwin

package supervisor

import (
	"errors"
	"testing"
)

func TestFileQARunIsUnsupportedOffDarwin(t *testing.T) {
	root := t.TempDir()
	r, err := New(Config{TempRoot: root}).Run(t.Context(), Request{Kind: GoResource, Mode: FileSmall, Budget: ResourceBudget{FileBytes: 64 << 10}})
	if !errors.Is(err, ErrUnsupported) || r.State != Failed || r.WorkRoot != "" || r.Worker.Entered || r.GoAuthorized || r.Usage.FileCandidate != "" || r.Usage.MonitoringStarted {
		t.Fatalf("non-Darwin file load looked successful: %+v %v", r, err)
	}
}
