//go:build darwin

package main

import (
	"fmt"
	"unsafe"

	"github.com/ebitengine/purego"
)

// proc_taskinfo, PROC_PIDTASKINFO=4, from Apple's xnu bsd/sys/proc_info.h.
// libproc returns the byte count: short reads are errors, never zero usage.
// CPU fields use Mach absolute time (fill_taskprocinfo/recount_times_mach);
// mach_timebase_info supplies the host-specific conversion to nanoseconds.
type taskInfo struct {
	Virtual, Resident, User, System, ThreadsUser, ThreadsSystem uint64
	Counters                                                    [12]int32
}

type processMonitor struct {
	pidinfo       func(int32, int32, uint64, unsafe.Pointer, int32) int32
	pidpath       func(int32, unsafe.Pointer, uint32) int32
	TimebaseNumer uint32
	TimebaseDenom uint32
}

type ProcessIdentity struct {
	PID               int    `json:"pid"`
	StartSeconds      uint64 `json:"start_seconds"`
	StartMicroseconds uint64 `json:"start_microseconds"`
}

func (m *processMonitor) identity(pid int) (ProcessIdentity, error) {
	var b struct {
		Fields                [12]uint32
		Comm                  [16]byte
		Name                  [32]byte
		Tail                  [6]uint32
		Seconds, Microseconds uint64
	}
	n := m.pidinfo(int32(pid), 3, 0, unsafe.Pointer(&b), int32(unsafe.Sizeof(b)))
	if n != int32(unsafe.Sizeof(b)) || b.Fields[3] != uint32(pid) {
		return ProcessIdentity{}, fmt.Errorf("proc_pidinfo BSDINFO invalid count/identity: %d", n)
	}
	return ProcessIdentity{pid, b.Seconds, b.Microseconds}, nil
}

func newProcessMonitor() (*processMonitor, error) {
	h, e := purego.Dlopen("/usr/lib/libproc.dylib", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if e != nil {
		return nil, e
	}
	m := new(processMonitor)
	purego.RegisterLibFunc(&m.pidinfo, h, "proc_pidinfo")
	purego.RegisterLibFunc(&m.pidpath, h, "proc_pidpath")
	system, e := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if e != nil {
		return nil, e
	}
	var timebase func(unsafe.Pointer) int32
	purego.RegisterLibFunc(&timebase, system, "mach_timebase_info")
	var tb struct{ Numer, Denom uint32 }
	if result := timebase(unsafe.Pointer(&tb)); result != 0 || tb.Numer == 0 || tb.Denom == 0 {
		return nil, fmt.Errorf("mach_timebase_info invalid: result=%d numer=%d denom=%d", result, tb.Numer, tb.Denom)
	}
	m.TimebaseNumer = tb.Numer
	m.TimebaseDenom = tb.Denom
	// Keep the library loaded for the process lifetime; callbacks retain its symbols.
	return m, nil
}
func (m *processMonitor) cpuSeconds(t taskInfo) float64 {
	return float64(t.User+t.System) * float64(m.TimebaseNumer) / float64(m.TimebaseDenom) / 1e9
}
func (m *processMonitor) sample(pid int) (taskInfo, error) {
	var t taskInfo
	n := m.pidinfo(int32(pid), 4, 0, unsafe.Pointer(&t), int32(unsafe.Sizeof(t)))
	if n != int32(unsafe.Sizeof(t)) {
		return t, fmt.Errorf("proc_pidinfo: got %d want %d", n, unsafe.Sizeof(t))
	}
	return t, nil
}
func (m *processMonitor) path(pid int) string {
	var b [4096]byte
	n := m.pidpath(int32(pid), unsafe.Pointer(&b[0]), uint32(len(b)))
	if n <= 0 {
		return ""
	}
	i := 0
	for i < len(b) && b[i] != 0 {
		i++
	}
	return string(b[:i])
}
