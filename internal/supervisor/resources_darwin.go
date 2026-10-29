//go:build darwin

package supervisor

import (
	"fmt"
	"math"
	"math/bits"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
)

// Apple proc_info.h PROC_PIDTASKINFO. Total includes terminated threads;
// pti_threads_* is the aggregate of currently existing threads only.
type resourceTaskInfo struct {
	Virtual, Resident, User, System, LiveUser, LiveSystem                                                                                uint64
	Policy, Faults, Pageins, COWFaults, MessagesSent, MessagesReceived, SyscallsMach, SyscallsUnix, Switches, Threads, Running, Priority int32
}
type resourceClock struct {
	now          func() uint64
	numer, denom uint32
}

var clockOnce sync.Once
var clockValue resourceClock
var clockErr error

func loadResourceClock() (*resourceClock, error) {
	clockOnce.Do(func() {
		h, e := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW|purego.RTLD_LOCAL)
		if e != nil {
			clockErr = e
			return
		}
		var info func(unsafe.Pointer) int32
		purego.RegisterLibFunc(&info, h, "mach_timebase_info")
		purego.RegisterLibFunc(&clockValue.now, h, "mach_absolute_time")
		var tb struct{ Numer, Denom uint32 }
		if n := info(unsafe.Pointer(&tb)); n != 0 || tb.Numer == 0 || tb.Denom == 0 {
			clockErr = fmt.Errorf("invalid Mach timebase result=%d numer=%d denom=%d", n, tb.Numer, tb.Denom)
			return
		}
		clockValue.numer, clockValue.denom = tb.Numer, tb.Denom
	})
	return &clockValue, clockErr
}
func machNanoseconds(ticks uint64, numer, denom uint32) (uint64, error) {
	if numer == 0 || denom == 0 {
		return 0, fmt.Errorf("invalid Mach timebase")
	}
	hi, lo := bits.Mul64(ticks, uint64(numer))
	if hi >= uint64(denom) {
		return 0, fmt.Errorf("Mach conversion overflow")
	}
	v, _ := bits.Div64(hi, lo, uint64(denom))
	return v, nil
}
func (c *resourceClock) stamp() (mono, ticks uint64, unixNS int64, err error) {
	ticks = c.now()
	mono, err = machNanoseconds(ticks, c.numer, c.denom)
	unixNS = time.Now().UnixNano()
	return
}

type resourceMeter struct {
	m            *identityMonitor
	clock        *resourceClock
	u            *ResourceUsage
	budget       ResourceBudget
	failureStart uint64
	lastSuccess  uint64
	firstSuccess uint64
	taskGone     bool
}

func newResourceMeter(m *identityMonitor, u *ResourceUsage, b ResourceBudget) (*resourceMeter, error) {
	c, e := loadResourceClock()
	if e != nil {
		return nil, fmt.Errorf("%w: %v", ErrMonitor, e)
	}
	u.TimebaseNumer, u.TimebaseDenom = c.numer, c.denom
	return &resourceMeter{m: m, clock: c, u: u, budget: b}, nil
}
func (r *resourceMeter) read(id Identity, phase string) ResourceSample {
	mono, ticks, wall, e := r.clock.stamp()
	s := ResourceSample{PID: id.PID, Phase: phase, MonotonicNS: mono, MachTicks: ticks, UnixNS: wall}
	if e != nil {
		s.Error = e.Error()
		return s
	}
	before, e := r.m.identity(id.PID, id.Nonce)
	if e != nil {
		s.Error = e.Error()
		return s
	}
	if e = classifyIdentity(id, before, nil); e != nil {
		s.Error = e.Error()
		return s
	}
	var t resourceTaskInfo
	n := r.m.info(int32(id.PID), 4, 0, unsafe.Pointer(&t), int32(unsafe.Sizeof(t)))
	if n != int32(unsafe.Sizeof(t)) {
		s.Error = fmt.Sprintf("PROC_PIDTASKINFO short read: %d want %d", n, unsafe.Sizeof(t))
		return s
	}
	s.RawMachUser, s.RawMachSystem, s.RawMachLiveThreadsUser, s.RawMachLiveThreadsSystem = t.User, t.System, t.LiveUser, t.LiveSystem
	s.RSSBytes, s.VirtualBytes, s.Threads = t.Resident, t.Virtual, t.Threads
	after, e := r.m.identity(id.PID, id.Nonce)
	if e != nil {
		s.Error = e.Error()
		return s
	}
	if e = classifyIdentity(id, after, nil); e != nil {
		s.Error = e.Error()
		return s
	}
	if math.MaxUint64-t.User < t.System {
		s.Error = "CPU raw sum overflow"
		return s
	}
	cpu, e := machNanoseconds(t.User+t.System, r.clock.numer, r.clock.denom)
	if e != nil || cpu > math.MaxInt64 {
		s.Error = "CPU Mach conversion overflow"
		return s
	}
	s.CPU = time.Duration(cpu)
	s.Valid = true
	return s
}
func (r *resourceMeter) record(s ResourceSample) error {
	if len(r.u.Samples) >= MaxResourceSamples {
		return fmt.Errorf("%w: sample capacity reached", ErrMonitor)
	}
	r.u.Samples = append(r.u.Samples, s)
	if s.Valid && s.Phase == "running" {
		// CPU is the latest absolute counter, not its maximum or a baseline delta.
		r.u.CPU = s.CPU
		if s.RSSBytes > r.u.PeakRSSBytes {
			r.u.PeakRSSBytes = s.RSSBytes
		}
		if s.VirtualBytes > r.u.PeakVirtualBytes {
			r.u.PeakVirtualBytes = s.VirtualBytes
		}
		if r.lastSuccess != 0 && s.MonotonicNS >= r.lastSuccess {
			gap := time.Duration(s.MonotonicNS - r.lastSuccess)
			if gap > r.u.MaxSampleGap {
				r.u.MaxSampleGap = gap
			}
		}
		if r.firstSuccess == 0 {
			r.firstSuccess = s.MonotonicNS
			if s.MonotonicNS >= r.u.GoMonotonicNS {
				r.u.GoToFirstSample = time.Duration(s.MonotonicNS - r.u.GoMonotonicNS)
			}
		}
		r.lastSuccess = s.MonotonicNS
	}
	if len(r.u.Samples) == MaxResourceSamples {
		return fmt.Errorf("%w: sample capacity reached", ErrMonitor)
	}
	return nil
}
func (r *resourceMeter) prepare(id Identity) error {
	s := r.read(id, "pre-go")
	if e := r.record(s); e != nil {
		return e
	}
	if !s.Valid {
		return fmt.Errorf("%w: pre-GO: %s", ErrMonitor, s.Error)
	}
	r.u.MonitoringReady = true
	return nil
}
func (r *resourceMeter) start() error {
	mono, ticks, wall, e := r.clock.stamp()
	if e != nil {
		return fmt.Errorf("%w: %v", ErrMonitor, e)
	}
	r.u.GoMonotonicNS, r.u.GoMachTicks, r.u.GoUnixNS = mono, ticks, wall
	r.u.MonitoringStarted = true
	return nil
}
func (r *resourceMeter) poll(id Identity) (string, error) {
	s := r.read(id, "running")
	if e := r.record(s); e != nil {
		return "monitor_error", e
	}
	if !s.Valid {
		if r.m.absent(id.PID) {
			r.taskGone = true
			return "", nil
		}
		if r.failureStart == 0 {
			r.failureStart = s.MonotonicNS
		}
		if s.MonotonicNS >= r.failureStart && time.Duration(s.MonotonicNS-r.failureStart) >= ResourceReadFailureWindow {
			return "monitor_error", fmt.Errorf("%w: continuous reads failed: %s", ErrMonitor, s.Error)
		}
		return "", nil
	}
	r.failureStart = 0
	if s.CPU > r.budget.CPU {
		r.u.CPUOvershoot = s.CPU - r.budget.CPU
		return "cpu_limit", ErrCPULimit
	}
	if s.RSSBytes > r.budget.RSSBytes {
		r.u.RSSOvershootBytes = s.RSSBytes - r.budget.RSSBytes
		return "rss_limit", ErrRSSLimit
	}
	return "", nil
}
func (r *resourceMeter) finish(v *receipt) error {
	if v == nil || !v.Waited {
		return nil
	}
	r.u.WaitMonotonicNS, r.u.WaitMachTicks, r.u.WaitUnixNS = v.WaitMonotonicNS, v.WaitMachTicks, v.WaitUnixNS
	mono, _, _, e := r.clock.stamp()
	if e != nil {
		return fmt.Errorf("%w: %v", ErrMonitor, e)
	}
	if r.u.ReceiptMonotonicNS == 0 {
		r.u.ReceiptMonotonicNS = mono
	}
	r.u.SignalAttemptMonotonicNS, r.u.SignalReturnMonotonicNS, r.u.SignalSent = v.SignalAttemptMonotonicNS, v.SignalReturnMonotonicNS, v.SignalSent
	if !r.u.MonitoringStarted {
		return nil
	}
	if v.WaitMonotonicNS < r.u.GoMonotonicNS || v.WaitMonotonicNS == 0 || mono < v.WaitMonotonicNS {
		return fmt.Errorf("%w: invalid monotonic Wait observation", ErrProtocol)
	}
	r.u.Wall = time.Duration(v.WaitMonotonicNS - r.u.GoMonotonicNS)
	r.u.ReceiptDelay = time.Duration(r.u.ReceiptMonotonicNS - v.WaitMonotonicNS)
	// A sample or failed read can arrive after the task has already been
	// waited, while COMPLETE is in flight or cleanup is running. Keep that raw
	// evidence, but it cannot describe a live task or extend its sample window.
	r.u.CPU = 0
	r.u.PeakRSSBytes = 0
	r.u.PeakVirtualBytes = 0
	r.u.MaxSampleGap = 0
	r.u.GoToFirstSample = 0
	r.u.LastSampleToWait = 0
	r.u.CPUOvershoot = 0
	r.u.RSSOvershootBytes = 0
	r.u.SuccessfulSamples = 0
	r.u.FirstSampleMonotonicNS = 0
	r.u.LastSampleMonotonicNS = 0
	for _, s := range r.u.Samples {
		if !s.Valid || s.Phase != "running" || s.MonotonicNS < r.u.GoMonotonicNS || s.MonotonicNS > v.WaitMonotonicNS {
			continue
		}
		r.u.CPU = s.CPU
		r.u.SuccessfulSamples++
		if s.RSSBytes > r.u.PeakRSSBytes {
			r.u.PeakRSSBytes = s.RSSBytes
		}
		if s.VirtualBytes > r.u.PeakVirtualBytes {
			r.u.PeakVirtualBytes = s.VirtualBytes
		}
		if r.u.FirstSampleMonotonicNS == 0 {
			r.u.FirstSampleMonotonicNS = s.MonotonicNS
			r.u.GoToFirstSample = time.Duration(s.MonotonicNS - r.u.GoMonotonicNS)
		}
		if r.u.LastSampleMonotonicNS != 0 {
			gap := time.Duration(s.MonotonicNS - r.u.LastSampleMonotonicNS)
			if gap > r.u.MaxSampleGap {
				r.u.MaxSampleGap = gap
			}
		}
		r.u.LastSampleMonotonicNS = s.MonotonicNS
		if s.CPU > r.budget.CPU && s.CPU-r.budget.CPU > r.u.CPUOvershoot {
			r.u.CPUOvershoot = s.CPU - r.budget.CPU
		}
	}
	if r.u.LastSampleMonotonicNS != 0 {
		r.u.LastSampleToWait = time.Duration(v.WaitMonotonicNS - r.u.LastSampleMonotonicNS)
	}
	if r.u.PeakRSSBytes > r.budget.RSSBytes {
		r.u.RSSOvershootBytes = r.u.PeakRSSBytes - r.budget.RSSBytes
	}
	if r.u.TriggerMonotonicNS > v.WaitMonotonicNS {
		r.u.LateStopReason, r.u.LateStopMonotonicNS = r.u.TriggerReason, r.u.TriggerMonotonicNS
		r.u.TriggerReason = ""
		r.u.TriggerMonotonicNS = 0
	}
	return nil
}
