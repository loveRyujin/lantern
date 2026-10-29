// Package supervisor supervises a sealed, fixed lifecycle helper. It does not
// execute user code and is not a sandbox for arbitrary code.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"
)

var (
	ErrUnsupported    = errors.New("supervisor platform unsupported")
	ErrInvalidRequest = errors.New("invalid supervisor request")
	ErrPeerLost       = errors.New("supervisor guardian lost")
	ErrProtocol       = errors.New("supervisor protocol failure")
)

type Kind string

const (
	GoLifecycle Kind = "go-lifecycle"
	GoResource  Kind = "go-resource"
)

type Mode string

const (
	Normal          Mode = "normal"
	ExpectedError   Mode = "error"
	Hold            Mode = "hold"
	SetSID          Mode = "setsid"
	SetPGID         Mode = "setpgid"
	ExitRace        Mode = "exit-cancel-race"
	CPUBusy         Mode = "cpu-busy"
	LowCPUSleep     Mode = "low-cpu-sleep"
	TouchMemory     Mode = "touch-memory"
	UntouchedMemory Mode = "untouched-memory"
	MultiThreadCPU  Mode = "multi-thread-cpu"
)

// Gate pauses a fixed lifecycle checkpoint for the trusted acceptance harness.
// It never grants permission to bypass registration, arming, or GO.
type Gate string

const (
	NoGate      Gate = ""
	BeforeSpawn Gate = "before-spawn"
	AfterSpawn  Gate = "after-spawn"
	Registered  Gate = "registered"
	Armed       Gate = "armed"
)

type Config struct {
	TempRoot string
	// EvidenceRoot retains write-only diagnostics. Protocol decisions use live
	// IPC exclusively. No worker is granted access to these diagnostics.
	EvidenceRoot string
}
type Request struct {
	Kind   Kind           `json:"kind"`
	Mode   Mode           `json:"mode"`
	Gate   Gate           `json:"gate,omitempty"`
	Budget ResourceBudget `json:"budget"`
}
type State string

const (
	Preparing       State = "preparing"
	StateRegistered State = "registered"
	StateArmed      State = "armed"
	Running         State = "running"
	Draining        State = "draining"
	Completed       State = "completed"
	Failed          State = "failed"
	Cancelled       State = "cancelled"
)

type Identity struct {
	PID               int    `json:"pid"`
	StartSeconds      uint64 `json:"start_seconds"`
	StartMicroseconds uint64 `json:"start_microseconds"`
	Image             string `json:"image"`
	Nonce             string `json:"nonce"`
}
type Event struct {
	Actor     string `json:"actor"`
	Seq       int    `json:"seq"`
	AtNS      int64  `json:"at_ns"`
	Kind      string `json:"kind"`
	PID       int    `json:"pid"`
	TargetPID int    `json:"target_pid,omitempty"`
	Detail    string `json:"detail,omitempty"`
}
type FDIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	Mode   uint32 `json:"mode"`
}
type WorkerResult struct {
	Entered       bool   `json:"entered"`
	PID           int    `json:"pid"`
	ExitCode      int    `json:"exit_code"`
	Signal        int    `json:"signal,omitempty"`
	Mode          Mode   `json:"mode"`
	Session       int    `json:"session,omitempty"`
	Group         int    `json:"group,omitempty"`
	OriginalGroup int    `json:"original_group,omitempty"`
	MappedBytes   uint64 `json:"mapped_bytes,omitempty"`
	TouchedBytes  uint64 `json:"touched_bytes,omitempty"`
	// Go may create its own poller descriptors. These are actual fstat identities,
	// so inherited credentials/control endpoints cannot hide behind FD reuse.
	EnvKeys             []string     `json:"env_keys,omitempty"`
	OpenFDs             []FDIdentity `json:"open_fds,omitempty"`
	ControlFDsInherited bool         `json:"control_fds_inherited"`
}
type Result struct {
	State              State          `json:"state"`
	Nonce              string         `json:"nonce"`
	HostPID            int            `json:"host_pid"`
	GuardianPID        int            `json:"guardian_pid"`
	Task               Identity       `json:"task"`
	Registered         bool           `json:"registered"`
	Armed              bool           `json:"armed"`
	GoAuthorized       bool           `json:"go_authorized"`
	Worker             WorkerResult   `json:"worker"`
	StopReason         string         `json:"stop_reason,omitempty"`
	HostWaitedGuardian bool           `json:"host_waited_guardian"`
	GuardianWaitedTask bool           `json:"guardian_waited_task"`
	TaskAbsent         bool           `json:"task_absent"`
	GuardianAbsent     bool           `json:"guardian_absent"`
	WorkRoot           string         `json:"work_root"`
	WorkRootClean      bool           `json:"work_root_clean"`
	EvidenceDir        string         `json:"evidence_dir,omitempty"`
	ControlFDs         []FDIdentity   `json:"control_fds,omitempty"`
	SignalSkipped      bool           `json:"signal_skipped"`
	SignalSkipReason   string         `json:"signal_skip_reason,omitempty"`
	SignalResolvedBy   string         `json:"signal_resolved_by,omitempty"`
	Events             []Event        `json:"events"`
	Wall               time.Duration  `json:"wall_ns"`
	Error              string         `json:"error,omitempty"`
	Budget             ResourceBudget `json:"budget"`
	Usage              ResourceUsage  `json:"usage"`
}

// Supervisor is immutable after construction and supports concurrent Runs.
type Supervisor struct{ config Config }

func New(c Config) *Supervisor {
	return &Supervisor{config: c}
}
func (s *Supervisor) Run(ctx context.Context, q Request) (Result, error) {
	if ctx == nil {
		return Result{State: Failed}, fmt.Errorf("%w: nil context", ErrInvalidRequest)
	}
	if err := validateRequest(s.config, q); err != nil {
		return Result{State: Failed}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{State: Cancelled}, err
	}
	if q.Kind == GoResource {
		q.Budget, _ = normalizeBudget(q.Budget)
	}
	return s.run(ctx, q)
}
func validateRequest(c Config, q Request) error {
	if !filepath.IsAbs(c.TempRoot) || filepath.Clean(c.TempRoot) != c.TempRoot || (c.EvidenceRoot != "" && (!filepath.IsAbs(c.EvidenceRoot) || filepath.Clean(c.EvidenceRoot) != c.EvidenceRoot)) {
		return fmt.Errorf("%w: absolute clean roots required", ErrInvalidRequest)
	}
	if q.Kind != GoLifecycle && q.Kind != GoResource {
		return fmt.Errorf("%w: unknown sealed helper", ErrInvalidRequest)
	}
	if q.Kind == GoResource {
		if q.Gate != NoGate {
			return fmt.Errorf("%w: resource helper requires NoGate", ErrInvalidRequest)
		}
		switch q.Mode {
		case Normal, CPUBusy, LowCPUSleep, TouchMemory, UntouchedMemory, MultiThreadCPU:
		default:
			return fmt.Errorf("%w: unknown resource load", ErrInvalidRequest)
		}
		_, err := normalizeBudget(q.Budget)
		return err
	}
	if q.Budget != (ResourceBudget{}) {
		return fmt.Errorf("%w: lifecycle helper has no resource budget", ErrInvalidRequest)
	}
	switch q.Mode {
	case Normal, ExpectedError, Hold, SetSID, SetPGID, ExitRace:
	default:
		return fmt.Errorf("%w: unknown fixed mode", ErrInvalidRequest)
	}
	switch q.Gate {
	case NoGate, BeforeSpawn, AfterSpawn, Registered, Armed:
	default:
		return fmt.Errorf("%w: unknown gate", ErrInvalidRequest)
	}
	return nil
}
