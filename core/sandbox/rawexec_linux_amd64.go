//go:build linux && amd64

package sandbox

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// All pointer-bearing storage is allocated and rooted before seccomp. Keep the
// complete filter/argv/env backing arrays reachable throughout the raw terminal.
type rawLaunchPlan struct {
	filters  []unix.SockFilter
	program  unix.SockFprog
	filename *byte
	argv     []*byte
	env      []*byte
	usage    unix.Rusage
	ready    [192]byte
}

func prepareRawLaunch(filename string, args, env []string) (*rawLaunchPlan, error) {
	p := &rawLaunchPlan{filters: buildFilter()}
	p.program = unix.SockFprog{Len: uint16(len(p.filters)), Filter: &p.filters[0]}
	var err error
	if p.filename, err = syscall.BytePtrFromString(filename); err != nil {
		return nil, err
	}
	if p.argv, err = syscall.SlicePtrFromStrings(args); err != nil {
		return nil, err
	}
	if p.env, err = syscall.SlicePtrFromStrings(env); err != nil {
		return nil, err
	}
	return p, nil
}

// rawLaunchTerminal executes only on the OS thread already locked by launcherMain.
// Landlock and this seccomp filter cover that thread and survive exec. Other Go
// bootstrap threads remain trusted and unfiltered; exec destroys them before
// user code runs. TSYNC would unnecessarily restrict those live runtime threads.
//
// After filter installation there are no allocations, scheduler-entering
// syscalls, JSON encoders, stack splits, normal returns, or calls into os/exec.
// launcher-only GODEBUG=asyncpreemptoff=1 also excludes asynchronous call-boundary
// preemption, which nosplit alone does not exclude.
// The raw usage sample keeps the CPU baseline after installation of all policy.
//
//go:nosplit
//go:norace
func rawLaunchTerminal(p *rawLaunchPlan) {
	result, _, errno := syscall.RawSyscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, 0, uintptr(unsafe.Pointer(&p.program)))
	if errno != 0 || result != 0 {
		rawLaunchFailure("{\"Error\":\"seccomp installation failed\"}\n")
	}
	_, _, errno = syscall.RawSyscall(unix.SYS_GETRUSAGE, 0, uintptr(unsafe.Pointer(&p.usage)), 0)
	if errno != 0 {
		rawLaunchFailure("{\"Error\":\"launcher usage sample failed\"}\n")
	}
	n := rawAppendString(&p.ready, 0, "{\"Ready\":true,\"LauncherPeakRSS\":")
	n = rawAppendUint(&p.ready, n, uint64(p.usage.Maxrss)*1024)
	n = rawAppendString(&p.ready, n, ",\"LauncherCPU\":")
	cpu := uint64(p.usage.Utime.Sec+p.usage.Stime.Sec)*1000000000 + uint64(p.usage.Utime.Usec+p.usage.Stime.Usec)*1000
	n = rawAppendUint(&p.ready, n, cpu)
	n = rawAppendString(&p.ready, n, "}\n")
	if !rawWriteAll(3, &p.ready[0], n) {
		rawLaunchFailure("{\"Error\":\"launcher Ready write failed\"}\n")
	}
	_, _, _ = syscall.RawSyscall(unix.SYS_EXECVE, uintptr(unsafe.Pointer(p.filename)), uintptr(unsafe.Pointer(&p.argv[0])), uintptr(unsafe.Pointer(&p.env[0])))
	rawLaunchFailure("{\"Error\":\"Python exec failed\"}\n")
}

//go:nosplit
//go:norace
func rawAppendString(buffer *[192]byte, n int, value string) int {
	for i := 0; i < len(value); i++ {
		buffer[n] = value[i]
		n++
	}
	return n
}

//go:nosplit
//go:norace
func rawAppendUint(buffer *[192]byte, n int, value uint64) int {
	var reversed [20]byte
	count := 0
	for {
		reversed[count] = byte(value%10) + '0'
		count++
		value /= 10
		if value == 0 {
			break
		}
	}
	for count > 0 {
		count--
		buffer[n] = reversed[count]
		n++
	}
	return n
}

//go:nosplit
//go:norace
func rawWriteAll(fd uintptr, data *byte, length int) bool {
	written := 0
	for written < length {
		n, _, e := syscall.RawSyscall(unix.SYS_WRITE, fd, uintptr(unsafe.Pointer(data))+uintptr(written), uintptr(length-written))
		if e == syscall.EINTR {
			continue
		}
		if e != 0 || n == 0 || n > uintptr(length-written) {
			return false
		}
		written += int(n)
	}
	return true
}

//go:nosplit
//go:norace
func rawLaunchFailure(message string) {
	_ = rawWriteAll(3, unsafe.StringData(message), len(message))
	for {
		_, _, _ = syscall.RawSyscall(unix.SYS_EXIT_GROUP, 125, 0, 0)
	}
}
