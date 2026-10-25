//go:build darwin

package supervisor

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/unix"
)

// Apple xnu bsd/sys/spawn.h and libsyscall/wrappers/spawn/spawn.h.
// CLOEXEC_DEFAULT closes every descriptor not explicitly selected by file
// actions, including third-party non-CLOEXEC host descriptors. SETEXEC replaces
// the launcher in-place, retaining its registered PID and birth identity.
const spawnCloseDefault = 0x4000
const spawnSetExec = 0x0040

type spawnAPI struct {
	initActions    func(*uintptr) int32
	destroyActions func(*uintptr) int32
	dup2           func(*uintptr, int32, int32) int32
	initAttr       func(*uintptr) int32
	destroyAttr    func(*uintptr) int32
	flags          func(*uintptr, int16) int32
	spawn          func(*int32, *byte, *uintptr, *uintptr, **byte, **byte) int32
}

var spawnOnce sync.Once
var spawnLib spawnAPI
var spawnErr error

func loadSpawn() (*spawnAPI, error) {
	spawnOnce.Do(func() {
		h, e := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW|purego.RTLD_LOCAL)
		if e != nil {
			spawnErr = e
			return
		}
		purego.RegisterLibFunc(&spawnLib.initActions, h, "posix_spawn_file_actions_init")
		purego.RegisterLibFunc(&spawnLib.destroyActions, h, "posix_spawn_file_actions_destroy")
		purego.RegisterLibFunc(&spawnLib.dup2, h, "posix_spawn_file_actions_adddup2")
		purego.RegisterLibFunc(&spawnLib.initAttr, h, "posix_spawnattr_init")
		purego.RegisterLibFunc(&spawnLib.destroyAttr, h, "posix_spawnattr_destroy")
		purego.RegisterLibFunc(&spawnLib.flags, h, "posix_spawnattr_setflags")
		purego.RegisterLibFunc(&spawnLib.spawn, h, "posix_spawn")
	})
	return &spawnLib, spawnErr
}

// spawnFixed is private: all callers use the sealed role/argv/env construction.
// Copies prevent dup2 action ordering from overwriting another source. Creating
// new CLOEXEC descriptors is local; no existing host descriptor flags change.
func spawnFixed(exe string, args []string, files []*os.File, replace bool) (*os.Process, error) {
	a, e := loadSpawn()
	if e != nil {
		return nil, e
	}
	var actions, attr uintptr
	if n := a.initActions(&actions); n != 0 {
		return nil, syscall.Errno(n)
	}
	defer a.destroyActions(&actions)
	if n := a.initAttr(&attr); n != 0 {
		return nil, syscall.Errno(n)
	}
	defer a.destroyAttr(&attr)
	flags := int16(spawnCloseDefault)
	if replace {
		flags |= spawnSetExec
	}
	if n := a.flags(&attr, flags); n != 0 {
		return nil, syscall.Errno(n)
	}
	var copies []int
	defer func() {
		for _, fd := range copies {
			_ = unix.Close(fd)
		}
	}()
	for target, f := range files {
		if f == nil {
			return nil, fmt.Errorf("nil whitelisted descriptor")
		}
		fd, e := unix.FcntlInt(f.Fd(), unix.F_DUPFD_CLOEXEC, 64)
		if e != nil {
			return nil, e
		}
		copies = append(copies, fd)
		if n := a.dup2(&actions, int32(fd), int32(target)); n != 0 {
			return nil, syscall.Errno(n)
		}
	}
	path, e := syscall.BytePtrFromString(exe)
	if e != nil {
		return nil, e
	}
	argv, e := stringPointers(append([]string{exe}, args...))
	if e != nil {
		return nil, e
	}
	env, e := stringPointers([]string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C", "GOMAXPROCS=2"})
	if e != nil {
		return nil, e
	}
	var pid int32
	n := a.spawn(&pid, path, &actions, &attr, &argv[0], &env[0])
	runtime.KeepAlive(files)
	runtime.KeepAlive(argv)
	runtime.KeepAlive(env)
	runtime.KeepAlive(path)
	if n != 0 {
		return nil, syscall.Errno(n)
	}
	if replace {
		return nil, fmt.Errorf("SETEXEC unexpectedly returned success")
	}
	return os.FindProcess(int(pid))
}
func stringPointers(v []string) ([]*byte, error) {
	p := make([]*byte, len(v)+1)
	for i, s := range v {
		b, e := syscall.BytePtrFromString(s)
		if e != nil {
			return nil, e
		}
		p[i] = b
	}
	return p, nil
}

type identityMonitor struct {
	info func(int32, int32, uint64, unsafe.Pointer, int32) int32
	path func(int32, unsafe.Pointer, uint32) int32
}

var monitorOnce sync.Once
var monitorLib identityMonitor
var monitorErr error

func monitor() (*identityMonitor, error) {
	monitorOnce.Do(func() {
		h, e := purego.Dlopen("/usr/lib/libproc.dylib", purego.RTLD_NOW|purego.RTLD_LOCAL)
		if e != nil {
			monitorErr = e
			return
		}
		purego.RegisterLibFunc(&monitorLib.info, h, "proc_pidinfo")
		purego.RegisterLibFunc(&monitorLib.path, h, "proc_pidpath")
	})
	return &monitorLib, monitorErr
}
func (m *identityMonitor) identity(pid int, nonce string) (Identity, error) {
	var b struct {
		Fields          [12]uint32
		Comm            [16]byte
		Name            [32]byte
		Tail            [6]uint32
		Seconds, Micros uint64
	}
	n := m.info(int32(pid), 3, 0, unsafe.Pointer(&b), int32(unsafe.Sizeof(b)))
	if n != int32(unsafe.Sizeof(b)) || b.Fields[3] != uint32(pid) || b.Seconds == 0 || b.Micros >= 1000000 {
		return Identity{}, fmt.Errorf("identity unavailable: %d", n)
	}
	var path [4096]byte
	n = m.path(int32(pid), unsafe.Pointer(&path[0]), uint32(len(path)))
	if n <= 0 {
		return Identity{}, fmt.Errorf("image unavailable")
	}
	i := 0
	for i < len(path) && path[i] != 0 {
		i++
	}
	return Identity{PID: pid, StartSeconds: b.Seconds, StartMicroseconds: b.Micros, Image: string(path[:i]), Nonce: nonce}, nil
}
func (m *identityMonitor) absent(pid int) bool {
	if pid <= 0 {
		return true
	}
	var b struct {
		Fields          [12]uint32
		Comm            [16]byte
		Name            [32]byte
		Tail            [6]uint32
		Seconds, Micros uint64
	}
	n := m.info(int32(pid), 3, 0, unsafe.Pointer(&b), int32(unsafe.Sizeof(b)))
	if n == int32(unsafe.Sizeof(b)) {
		return false
	}
	return syscall.Kill(pid, 0) == syscall.ESRCH
}
