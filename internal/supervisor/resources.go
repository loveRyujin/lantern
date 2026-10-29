package supervisor

import (
	"errors"
	"fmt"
	"time"
)

var (
	ErrCPULimit  = errors.New("supervisor CPU budget exceeded")
	ErrRSSLimit  = errors.New("supervisor sampled RSS budget exceeded")
	ErrWallLimit = errors.New("supervisor wall budget exceeded")
	ErrMonitor   = errors.New("supervisor resource monitor failed")
)

const (
	ResourceSampleInterval    = 10 * time.Millisecond
	ResourceReadFailureWindow = 50 * time.Millisecond
	MaxResourceSamples        = 1024
)

// These defaults and request ceilings belong to the sealed acceptance helper,
// not a future SQL/Python service. Zero means the default for that one field.
type ResourceBudget struct {
	CPU      time.Duration `json:"cpu_ns"`
	RSSBytes uint64        `json:"rss_bytes"`
	Wall     time.Duration `json:"wall_ns"`
}

func normalizeBudget(b ResourceBudget) (ResourceBudget, error) {
	if b.CPU < 0 || b.Wall < 0 || b.CPU > 3*time.Second || b.RSSBytes > 512<<20 || b.Wall > 5*time.Second {
		return ResourceBudget{}, fmt.Errorf("%w: resource request exceeds sealed helper bounds", ErrInvalidRequest)
	}
	if b.CPU == 0 {
		b.CPU = time.Second
	}
	if b.RSSBytes == 0 {
		b.RSSBytes = 128 << 20
	}
	if b.Wall == 0 {
		b.Wall = 3 * time.Second
	}
	return b, nil
}

type ResourceSample struct {
	PID                      int           `json:"pid"`
	Phase                    string        `json:"phase"`
	MonotonicNS              uint64        `json:"monotonic_ns"`
	MachTicks                uint64        `json:"mach_ticks"`
	UnixNS                   int64         `json:"unix_ns"`
	Valid                    bool          `json:"valid"`
	Error                    string        `json:"error,omitempty"`
	RawMachUser              uint64        `json:"raw_mach_user"`
	RawMachSystem            uint64        `json:"raw_mach_system"`
	RawMachLiveThreadsUser   uint64        `json:"raw_mach_live_threads_user"`
	RawMachLiveThreadsSystem uint64        `json:"raw_mach_live_threads_system"`
	CPU                      time.Duration `json:"cpu_ns"`
	RSSBytes                 uint64        `json:"rss_bytes"`
	VirtualBytes             uint64        `json:"virtual_bytes"`
	Threads                  int32         `json:"threads"`
}
type ResourceUsage struct {
	MonitoringReady          bool             `json:"monitoring_ready"`
	MonitoringStarted        bool             `json:"monitoring_started"`
	TimebaseNumer            uint32           `json:"timebase_numer"`
	TimebaseDenom            uint32           `json:"timebase_denom"`
	GoMonotonicNS            uint64           `json:"go_monotonic_ns"`
	GoMachTicks              uint64           `json:"go_mach_ticks"`
	GoUnixNS                 int64            `json:"go_unix_ns"`
	WaitMonotonicNS          uint64           `json:"wait_monotonic_ns"`
	WaitMachTicks            uint64           `json:"wait_mach_ticks"`
	WaitUnixNS               int64            `json:"wait_unix_ns"`
	ReceiptMonotonicNS       uint64           `json:"receipt_monotonic_ns"`
	Wall                     time.Duration    `json:"wall_ns"`
	ReceiptDelay             time.Duration    `json:"receipt_delay_ns"`
	CPU                      time.Duration    `json:"cpu_ns"`
	PeakRSSBytes             uint64           `json:"peak_rss_bytes"`
	PeakVirtualBytes         uint64           `json:"peak_virtual_bytes"`
	MaxSampleGap             time.Duration    `json:"max_sample_gap_ns"`
	GoToFirstSample          time.Duration    `json:"go_to_first_sample_ns"`
	LastSampleToWait         time.Duration    `json:"last_sample_to_wait_ns"`
	SuccessfulSamples        int              `json:"successful_samples"`
	FirstSampleMonotonicNS   uint64           `json:"first_sample_monotonic_ns"`
	LastSampleMonotonicNS    uint64           `json:"last_sample_monotonic_ns"`
	TriggerMonotonicNS       uint64           `json:"trigger_monotonic_ns"`
	TriggerReason            string           `json:"trigger_reason,omitempty"`
	LateStopReason           string           `json:"late_stop_reason,omitempty"`
	LateStopMonotonicNS      uint64           `json:"late_stop_monotonic_ns,omitempty"`
	CPUOvershoot             time.Duration    `json:"cpu_overshoot_ns"`
	RSSOvershootBytes        uint64           `json:"rss_overshoot_bytes"`
	SignalAttemptMonotonicNS uint64           `json:"signal_attempt_monotonic_ns"`
	SignalReturnMonotonicNS  uint64           `json:"signal_return_monotonic_ns"`
	SignalSent               bool             `json:"signal_sent"`
	Samples                  []ResourceSample `json:"samples"`
}

func resourceCause(reason string) error {
	switch reason {
	case "cpu_limit":
		return ErrCPULimit
	case "rss_limit":
		return ErrRSSLimit
	case "wall_limit":
		return ErrWallLimit
	case "monitor_error":
		return ErrMonitor
	}
	return nil
}
