//go:build linux && amd64

package sandbox

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"golang.org/x/sys/unix"
)

const guardianArg = "--lantern-sandbox-guardian-v1"
const launcherArg = "--lantern-sandbox-launcher-v1"

type launchRequest struct {
	Root   string
	Plan   runtimePlan
	Limits Limits
}
type setupMessage struct {
	Ready           bool
	Error           string
	LauncherPeakRSS uint64
	LauncherCPU     time.Duration
}

func init() {
	if len(os.Args) != 2 {
		return
	}
	switch os.Args[1] {
	case guardianArg:
		os.Exit(guardianMain())
	case launcherArg:
		os.Exit(launcherMain())
	}
}
func launcherMain() int {
	status := os.NewFile(3, "setup-status")
	fail := func(err error) int { _ = json.NewEncoder(status).Encode(setupMessage{Error: err.Error()}); return 125 }
	if err := checkUnprivileged(); err != nil {
		return fail(err)
	}
	var req launchRequest
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 2<<20)).Decode(&req); err != nil {
		return fail(err)
	}
	runtime.LockOSThread() // survives until exec; Landlock is applied to this thread.
	if err := unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(unix.SIGKILL), 0, 0, 0); err != nil {
		return fail(err)
	}
	if os.Getppid() == 1 {
		return fail(fmt.Errorf("guardian exited before launcher setup"))
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fail(err)
	}
	// Fixed CPU hard backstop, not an RSS quota. Sampling below preserves fractional CPU requests.
	seconds := uint64((req.Limits.CPU + time.Second - 1) / time.Second)
	for resource, lim := range map[int]unix.Rlimit{unix.RLIMIT_CORE: {}, unix.RLIMIT_CPU: {Cur: seconds, Max: seconds + 1}, unix.RLIMIT_FSIZE: {Cur: 5 << 20, Max: 5 << 20}, unix.RLIMIT_NOFILE: {Cur: 64, Max: 64}} {
		if err := unix.Setrlimit(resource, &lim); err != nil {
			return fail(err)
		}
	}
	null, err := unix.Open("/dev/null", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fail(err)
	}
	if err = unix.Dup2(null, 0); err != nil {
		return fail(err)
	}
	unix.Close(null)
	// Keep only the setup-status fd until exec; FD_CLOEXEC removes it atomically.
	if err = unix.CloseRange(4, ^uint(0), unix.CLOSE_RANGE_UNSHARE); err != nil {
		return fail(err)
	}
	unix.CloseOnExec(3)
	if err = unix.Chdir(req.Root); err != nil {
		return fail(err)
	}
	if err = installFiles(req.Root, req.Plan); err != nil {
		return fail(err)
	}
	// Report startup high-water separately; it must not be called Python RSS.
	var ru unix.Rusage
	_ = unix.Getrusage(unix.RUSAGE_SELF, &ru)
	if err = json.NewEncoder(status).Encode(setupMessage{Ready: true, LauncherPeakRSS: uint64(ru.Maxrss) * 1024, LauncherCPU: time.Duration(ru.Utime.Sec+ru.Stime.Sec)*time.Second + time.Duration(ru.Utime.Usec+ru.Stime.Usec)*time.Microsecond}); err != nil {
		return 125
	}
	if err = installFilter(); err != nil {
		return fail(err)
	}
	if err = unix.Exec(req.Plan.Python, []string{req.Plan.Python, "-I", "-B", filepath.Join(req.Root, "in/__code__.py")}, pythonEnv(req.Root)); err != nil {
		return fail(err)
	}
	return 125
}
