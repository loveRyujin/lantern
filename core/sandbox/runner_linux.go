//go:build linux && amd64

package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/loveRyujin/lantern/core/errcode"
	"golang.org/x/sys/unix"
)

var runSlots = make(chan struct{}, 2)

type guardianRequest struct {
	Spec Spec
	Plan runtimePlan
	Root string
}
type guardianReply struct {
	Result Result
	Code   errcode.Code
	Detail string
}

func (r *Runner) run(ctx context.Context, spec Spec) (Result, error) {
	if err := checkUnprivileged(); err != nil {
		return align(Result{}, SandboxUnavailable(err.Error()))
	}
	select {
	case runSlots <- struct{}{}:
		defer func() { <-runSlots }()
	case <-ctx.Done():
		return align(Result{}, Timeout(ctx.Err().Error()))
	}
	plan, err := prepareRuntime(ctx, spec.Runtime, spec.ExtraReadOnly)
	if err != nil {
		if ctx.Err() != nil {
			return align(Result{}, Timeout(ctx.Err().Error()))
		}
		return align(Result{}, err)
	}
	tempRoot := r.config.TempRoot
	if tempRoot != "" {
		tempRoot, err = filepath.Abs(tempRoot)
		if err != nil {
			return align(Result{}, Failed(err.Error()))
		}
		if err = os.MkdirAll(tempRoot, 0700); err != nil {
			return align(Result{}, Failed(err.Error()))
		}
	}
	root, err := os.MkdirTemp(tempRoot, "lantern-exec-")
	if err != nil {
		return align(Result{}, Failed(err.Error()))
	}
	defer os.RemoveAll(root) // fallback cleanup if the trusted guardian itself fails
	exe, err := os.Executable()
	if err != nil {
		return align(Result{}, SandboxUnavailable(err.Error()))
	}
	read, write, err := os.Pipe()
	if err != nil {
		return align(Result{}, Failed(err.Error()))
	}
	defer read.Close()
	defer write.Close()
	cmd := exec.Command(exe, guardianArg)
	cmd.Stdin = read
	cmd.Env = []string{"GOMAXPROCS=2", "GOMEMLIMIT=64MiB"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	output := cappedStream{limit: 32 << 20}
	var diagnostic cappedStream
	cmd.Stdout = &output
	cmd.Stderr = &diagnostic
	if err = cmd.Start(); err != nil {
		return align(Result{}, SandboxUnavailable(err.Error()))
	}
	read.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	if err = json.NewEncoder(write).Encode(guardianRequest{spec, plan, root}); err != nil {
		write.Close()
		<-done
		return align(Result{}, Failed(err.Error()))
	}
	var waitErr error
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		write.Close()
		select {
		case waitErr = <-done:
		case <-time.After(5 * time.Second):
			_ = unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
			waitErr = <-done
		}
	}
	var reply guardianReply
	err = json.Unmarshal([]byte(output.String()), &reply)
	// Cancellation can kill the launcher before Ready, or interrupt the guardian
	// before its reply. Wait/cleanup has finished above; missing setup state must
	// not turn a caller cancellation into an isolation-installation failure.
	if ctx.Err() != nil {
		reply.Result.Artifacts = nil
		return align(reply.Result, Timeout(ctx.Err().Error()))
	}
	if err != nil || waitErr != nil {
		return align(Result{}, Failed(fmt.Sprintf("guardian failed: %v JSON: %v %s", waitErr, err, diagnostic.String())))
	}
	if reply.Code != "" {
		return align(reply.Result, &ExecError{Kind: reply.Code, Detail: reply.Detail})
	}
	return reply.Result, nil
}

type cappedStream struct {
	limit    int
	mu       sync.Mutex
	buf      bytes.Buffer
	overflow atomic.Bool
}

func (w *cappedStream) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	limit := w.limit
	if limit == 0 {
		limit = maxStreamBytes
	}
	remaining := limit - w.buf.Len()
	if len(p) > remaining {
		w.overflow.Store(true)
		p = p[:remaining]
	}
	_, _ = w.buf.Write(p)
	return n, nil
}
func (w *cappedStream) String() string { w.mu.Lock(); defer w.mu.Unlock(); return w.buf.String() }

func guardianMain() int {
	decoder := json.NewDecoder(io.LimitReader(os.Stdin, 32<<20))
	var req guardianRequest
	if err := decoder.Decode(&req); err != nil {
		_ = json.NewEncoder(os.Stdout).Encode(guardianReply{Code: errcode.SandboxUnavailable, Detail: "invalid bounded guardian request"})
		return 0
	}
	spec, err := normalize(req.Spec)
	if err != nil {
		_ = json.NewEncoder(os.Stdout).Encode(guardianReply{Code: errcode.SandboxUnavailable, Detail: err.Error()})
		return 0
	}
	req.Spec = spec
	parentGone := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, io.MultiReader(decoder.Buffered(), os.Stdin)); close(parentGone) }()
	reply := executeGuarded(req, parentGone)
	_ = json.NewEncoder(os.Stdout).Encode(reply)
	return 0
}
func executeGuarded(req guardianRequest, parentGone <-chan struct{}) (reply guardianReply) {
	root := req.Root
	var err error
	// Return only after cleanup, including all validation/error paths.
	defer func() {
		if err := os.RemoveAll(root); err != nil {
			reply.Result.Artifacts = nil
			reply.Code = errcode.Exec
			reply.Detail = "execution directory cleanup: " + err.Error()
		}
	}()
	for _, d := range []string{"in", "out", "tmp"} {
		if err = os.Mkdir(filepath.Join(root, d), 0700); err != nil {
			return guardianReply{Code: errcode.Exec, Detail: err.Error()}
		}
	}
	if len(req.Plan.FontCache) > 0 {
		cacheDir := filepath.Join(root, "tmp", "mpl")
		if err = os.Mkdir(cacheDir, 0700); err != nil {
			return guardianReply{Code: errcode.Exec, Detail: err.Error()}
		}
		for name, data := range req.Plan.FontCache {
			if !validFontSeed(name, data) {
				return guardianReply{Code: errcode.SandboxUnavailable, Detail: "invalid font-cache seed"}
			}
			if err = os.WriteFile(filepath.Join(cacheDir, name), data, 0600); err != nil {
				return guardianReply{Code: errcode.Exec, Detail: err.Error()}
			}
		}
	}
	if _, ok := req.Spec.Inputs["__code__.py"]; ok {
		return guardianReply{Code: errcode.Exec, Detail: "reserved input name __code__.py"}
	}
	for name, data := range req.Spec.Inputs {
		p := filepath.Join(root, "in", filepath.FromSlash(name))
		if err = os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			return guardianReply{Code: errcode.Exec, Detail: err.Error()}
		}
		if err = os.WriteFile(p, data, 0600); err != nil {
			return guardianReply{Code: errcode.Exec, Detail: err.Error()}
		}
	}
	if err = os.WriteFile(filepath.Join(root, "in/__code__.py"), []byte(req.Spec.Code), 0600); err != nil {
		return guardianReply{Code: errcode.Exec, Detail: err.Error()}
	}
	exe, err := os.Executable()
	if err != nil {
		return guardianReply{Code: errcode.SandboxUnavailable, Detail: err.Error()}
	}
	statusRead, statusWrite, err := os.Pipe()
	if err != nil {
		return guardianReply{Code: errcode.SandboxUnavailable, Detail: err.Error()}
	}
	defer statusRead.Close()
	defer statusWrite.Close()
	data, err := json.Marshal(launchRequest{root, req.Plan, req.Spec.Limits})
	if err != nil || len(data) > 2<<20 {
		return guardianReply{Code: errcode.SandboxUnavailable, Detail: "runtime plan exceeds launcher budget"}
	}
	cmd := exec.Command(exe, launcherArg)
	cmd.Stdin = bytes.NewReader(data)
	// The filtered terminal chain is deeply nosplit and uses RawSyscall only.
	// Go nevertheless permits asynchronous preemption at call boundaries in
	// nosplit functions. Disable it for this trusted launcher so that the
	// filtered OS thread never re-enters the Go scheduler before exec/exit.
	// This launcher-only variable is absent from pythonEnv's allowlist.
	cmd.Env = []string{"GOMAXPROCS=2", "GOMEMLIMIT=64MiB", "GODEBUG=asyncpreemptoff=1"}
	cmd.ExtraFiles = []*os.File{statusWrite}
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	var out, stderr cappedStream
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	runtime.LockOSThread() // Pdeathsig belongs to the creating OS thread, not any Go goroutine.
	defer runtime.UnlockOSThread()
	if err = cmd.Start(); err != nil {
		return guardianReply{Code: errcode.SandboxUnavailable, Detail: err.Error()}
	}
	statusWrite.Close()
	statusDone := make(chan []setupMessage, 1)
	go func() {
		var messages []setupMessage
		d := json.NewDecoder(io.LimitReader(statusRead, 4096))
		for {
			var m setupMessage
			if d.Decode(&m) != nil {
				break
			}
			messages = append(messages, m)
		}
		statusDone <- messages
	}()
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	started := time.Now()
	var lastSample time.Time
	ticker := time.NewTicker(samplePeriod)
	defer ticker.Stop()
	usage := Usage{PythonPID: cmd.Process.Pid}
	var waitErr error
	var reason error
	var messages []setupMessage
	var statusReceived bool
	var launcherCPU time.Duration
	var inspectionSince time.Time
	var usageInspectionSince time.Time
	var untrusted bool
	for running := true; running; {
		select {
		case messages = <-statusDone:
			statusReceived = true
			for _, m := range messages {
				if m.Ready {
					launcherCPU = m.LauncherCPU
					untrusted = true
				}
				if m.Error != "" {
					untrusted = false
					break
				}
			}
			if untrusted {
				lastSample = time.Now()
			}
			statusDone = nil
		case waitErr = <-waited:
			running = false
		case <-parentGone:
			reason = Timeout("parent liveness pipe closed")
			_ = cmd.Process.Kill()
			waitErr = <-waited
			running = false
		case <-ticker.C:
			now := time.Now()
			if now.Sub(started) > req.Spec.Limits.Wall {
				reason = Timeout("wall limit exceeded")
			}
			// EOF on the CLOEXEC setup pipe plus Ready confirms the exec handoff.
			// The same untrusted PID remains monitored across every later exec,
			// including direct ELF-loader invocation; executable names are not a
			// resource-enforcement boundary.
			if untrusted {
				rss, hwm, cpu, e := sampleTask(cmd.Process.Pid)
				if e == nil {
					usageInspectionSince = time.Time{}
					if gap := now.Sub(lastSample); gap > usage.MaxSampleGap {
						usage.MaxSampleGap = gap
					}
					lastSample = now
					usage.SampleCount++
					if hwm > usage.PeakRSS {
						usage.PeakRSS = hwm
					}
					if rss > usage.PeakRSS {
						usage.PeakRSS = rss
					}
					if cpu >= launcherCPU {
						cpu -= launcherCPU
					} else {
						cpu = 0
					}
					usage.CPU = cpu
					if rss > req.Spec.Limits.Memory || hwm > req.Spec.Limits.Memory {
						reason = OOM("sampled Python RSS limit exceeded")
					}
					if cpu > req.Spec.Limits.CPU {
						reason = Timeout("sampled CPU limit exceeded")
					}
				} else {
					if usageInspectionSince.IsZero() {
						usageInspectionSince = now
					}
					if now.Sub(usageInspectionSince) >= 2*samplePeriod {
						reason = Failed("task resource inspection unavailable: " + e.Error())
					}
				}
			}
			if out.overflow.Load() || stderr.overflow.Load() {
				reason = Failed("stdout/stderr exceeds 64 KiB")
			}
			if e := checkWorkspace(root, cmd.Process.Pid); e != nil {
				if errors.Is(e, errTaskFDNotReady) {
					if inspectionSince.IsZero() {
						inspectionSince = now
					}
					// Exit teardown may hide proc fds before Wait is delivered.
					// Retry only a bounded observation window; a live inaccessible
					// task still fails closed rather than bypassing disk accounting.
					if now.Sub(inspectionSince) >= 2*samplePeriod {
						reason = Failed(e.Error())
					}
				} else {
					reason = Failed(e.Error())
				}
			} else {
				inspectionSince = time.Time{}
			}
			if reason != nil {
				_ = cmd.Process.Kill()
				waitErr = <-waited
				running = false
			}
		}
	}
	if !statusReceived {
		messages = <-statusDone
	}
	usage.Wall = time.Since(started)
	// wait4 covers short-lived Python tasks missed by polling. Its RSS includes the
	// trusted launcher; keep it separate, and compare only increases above launcher peak.
	var launcherPeak uint64
	var ready bool
	var setupErr string
	for _, m := range messages {
		if m.Ready {
			ready = true
			launcherPeak = m.LauncherPeakRSS
			launcherCPU = m.LauncherCPU
		}
		if m.Error != "" {
			setupErr = m.Error
		}
	}
	usage.LauncherPeakRSS = launcherPeak
	usage.LauncherCPU = launcherCPU
	if cmd.ProcessState != nil {
		usage.WaitPeakRSS = uint64(cmd.ProcessState.SysUsage().(*syscall.Rusage).Maxrss) * 1024
		usage.CPU = cmd.ProcessState.UserTime() + cmd.ProcessState.SystemTime() - launcherCPU
		if usage.CPU < 0 {
			usage.CPU = 0
		}
	}
	reply.Result = Result{RuntimeWarnings: req.Plan.Warnings, Stdout: out.String(), Stderr: stderr.String(), Usage: usage}
	if setupErr != "" || !ready {
		return guardianReply{Result: Result{Usage: usage}, Code: errcode.SandboxUnavailable, Detail: "launcher: " + setupErr}
	}
	if usage.WaitPeakRSS > req.Spec.Limits.Memory && usage.WaitPeakRSS > launcherPeak {
		reason = OOM("wait4 high-water exceeds RSS limit and trusted startup peak")
	}
	if reason == nil && usage.CPU > req.Spec.Limits.CPU {
		reason = Timeout("CPU limit exceeded")
	}
	if reason == nil && (out.overflow.Load() || stderr.overflow.Load()) {
		reason = Failed("stdout/stderr exceeds 64 KiB")
	}
	if reason == nil {
		reason = checkWorkspace(root, 0)
	}
	if reason == nil && waitErr != nil {
		if state, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && state.Signaled() {
			if state.Signal() == syscall.SIGXCPU {
				reason = Timeout("RLIMIT_CPU exceeded")
			} else {
				reason = Killed(state.Signal().String())
			}
		} else {
			reason = Failed("Python exited nonzero")
		}
	}
	if reason != nil {
		var coded interface{ Code() errcode.Code }
		if e, ok := reason.(interface{ Code() errcode.Code }); ok {
			coded = e
		}
		reply.Code = errcode.Exec
		if coded != nil {
			reply.Code = coded.Code()
		}
		reply.Detail = reason.Error()
		return reply
	}
	artifacts, err := collectArtifacts(filepath.Join(root, "out"), req.Spec.Limits.MaxOutput)
	if err != nil {
		reply.Code = errcode.Exec
		reply.Detail = err.Error()
		return reply
	}
	reply.Result.Artifacts = artifacts
	return reply
}
