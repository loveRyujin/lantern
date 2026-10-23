package sandbox

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"
)

// Config configures trusted host storage, never sandbox permissions.
type Config struct{ TempRoot string }

// Runner executes the strict single-task Python profile. It is safe for concurrent use.
type Runner struct{ config Config }

// New constructs the platform runner. Unsupported platforms fail closed in Run.
func New(config Config) *Runner { return &Runner{config: config} }

var _ Sandbox = (*Runner)(nil)

// Usage is observed usage of the untrusted Python task, excluding Go supervisors.
// RSS and directory budgets are sampled, not kernel hard quotas. MaxSampleGap
// records the largest actual gap, so scheduler delays remain visible.
type Usage struct {
	// WaitPeakRSS includes trusted launcher startup; it is not pure Python RSS.
	WaitPeakRSS     uint64
	LauncherPeakRSS uint64
	LauncherCPU     time.Duration
	PeakRSS         uint64
	CPU             time.Duration
	Wall            time.Duration
	MaxSampleGap    time.Duration
	PythonPID       int
	SampleCount     uint64
}

const (
	defaultMemory     = uint64(512 << 20)
	hardMemory        = uint64(1 << 30)
	maxCode           = 1 << 20
	maxInputs         = 16 << 20
	maxWorkBytes      = int64(64 << 20)
	maxWorkEntries    = 1024
	maxArtifacts      = 16
	maxArtifactsBytes = 20 << 20
	maxStreamBytes    = 64 << 10
	samplePeriod      = 10 * time.Millisecond
)

func normalize(spec Spec) (Spec, error) {
	if len(spec.Inputs) > maxWorkEntries {
		return spec, fmt.Errorf("%w: inputs exceed 1024 entries", ErrInvalidSpec)
	}
	if err := ValidateSpec(spec); err != nil {
		return spec, err
	}
	if len(spec.Code) > maxCode {
		return spec, fmt.Errorf("%w: code exceeds 1 MiB", ErrInvalidSpec)
	}
	if _, ok := spec.Inputs["__code__.py"]; ok {
		return spec, fmt.Errorf("%w: reserved input name __code__.py", ErrInvalidSpec)
	}
	total := 0
	nameBytes := 0
	inputEntries := make(map[string]bool)
	for name, v := range spec.Inputs {
		nameBytes += len(name)
		if len(name) > 4096 || nameBytes > 128<<10 {
			return spec, fmt.Errorf("%w: input path metadata exceeds budget", ErrInvalidSpec)
		}
		if strings.HasPrefix(name, "__code__.py/") {
			return spec, fmt.Errorf("%w: reserved input name __code__.py", ErrInvalidSpec)
		}
		for entry := name; entry != "."; entry = path.Dir(entry) {
			inputEntries[entry] = true
			if len(inputEntries) > maxWorkEntries {
				return spec, fmt.Errorf("%w: inputs exceed 1024 files/directories", ErrInvalidSpec)
			}
		}
		if len(v) > maxInputs-total {
			return spec, fmt.Errorf("%w: inputs exceed 16 MiB", ErrInvalidSpec)
		}
		total += len(v)
	}
	if spec.Limits.NProc > 1 {
		return spec, fmt.Errorf("%w: Linux single-task profile accepts NProc 0 or 1", ErrInvalidSpec)
	}
	if spec.Limits.CPU > 30*time.Second || spec.Limits.Wall > 60*time.Second || spec.Limits.Memory > hardMemory {
		return spec, fmt.Errorf("%w: resource request exceeds hard request ceiling", ErrInvalidSpec)
	}
	if spec.Limits.CPU == 0 {
		spec.Limits.CPU = 10 * time.Second
	}
	if spec.Limits.Wall == 0 {
		spec.Limits.Wall = 30 * time.Second
	}
	if spec.Limits.Memory == 0 {
		spec.Limits.Memory = defaultMemory
	}
	spec.Limits.NProc = 1
	spec.Limits.MaxOutput = artifactLimit(spec.Limits.MaxOutput)
	return cloneSpec(spec), nil
}
func (r *Runner) Run(ctx context.Context, spec Spec) (Result, error) {
	if ctx == nil {
		return Result{}, fmt.Errorf("sandbox: nil context")
	}
	spec, err := normalize(spec)
	if err != nil {
		return Result{}, err
	}
	if err = ctx.Err(); err != nil {
		return align(Result{Limits: spec.Limits}, Timeout(err.Error()))
	}
	result, err := r.run(ctx, spec)
	result.Limits = spec.Limits
	return result, err
}
