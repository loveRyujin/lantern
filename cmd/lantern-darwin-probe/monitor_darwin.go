//go:build darwin

package main

import (
	"fmt"
	"unsafe"

	"github.com/ebitengine/purego"
)

// proc_taskinfo, PROC_PIDTASKINFO=4, from Apple's xnu bsd/sys/proc_info.h.
// libproc returns the byte count: short reads are errors, never zero usage.
// CPU fields are nanoseconds (proc_filltaskinfo converts Mach absolute time).
type taskInfo struct {
	Virtual, Resident, User, System, ThreadsUser, ThreadsSystem uint64
	Counters                                                    [12]int32
}

type processMonitor struct {
	pidinfo func(int32, int32, uint64, unsafe.Pointer, int32) int32
	pidpath func(int32, unsafe.Pointer, uint32) int32
}

func newProcessMonitor() (*processMonitor, error) {
	h, e := purego.Dlopen("/usr/lib/libproc.dylib", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if e != nil {
		return nil, e
	}
	m := new(processMonitor)
	purego.RegisterLibFunc(&m.pidinfo, h, "proc_pidinfo")
	purego.RegisterLibFunc(&m.pidpath, h, "proc_pidpath")
	// Keep the library loaded for the process lifetime; callbacks retain its symbols.
	return m, nil
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
