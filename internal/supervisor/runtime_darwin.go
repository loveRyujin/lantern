//go:build darwin

package supervisor

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const helperArgument = "--lantern-internal-supervisor-v1"

// This dispatch runs only in a newly exec'd sealed helper. Run never exits,
// execs, changes cwd/environment/rlimits, or installs handlers in the host.
func init() {
	if len(os.Args) >= 3 && os.Args[1] == helperArgument {
		code := 125
		switch os.Args[2] {
		case "worker":
			if len(os.Args) == 4 {
				code = workerMain(Mode(os.Args[3]))
			}
		case "guardian", "launcher":
			if len(os.Args) == 4 {
				code = helperMain(os.Args[2], os.Args[3])
			}
		}
		os.Exit(code)
	}
}

type jobConfig struct {
	Nonce          string       `json:"nonce"`
	Request        Request      `json:"request"`
	Work           string       `json:"work"`
	Evidence       string       `json:"evidence"`
	RetainEvidence bool         `json:"retain_evidence"`
	ControlFDs     []FDIdentity `json:"control_fds"`
}
type incoming struct {
	Message message
	Err     error
}

func messages(f *os.File, nonce string) <-chan incoming {
	out := make(chan incoming, 8)
	go func() {
		defer close(out)
		s := bufio.NewScanner(f)
		s.Buffer(make([]byte, 512), 4097)
		seq := 0
		for s.Scan() {
			seq++
			v, e := decodeMessage(s.Bytes(), nonce, seq)
			out <- incoming{v, e}
			if e != nil {
				return
			}
		}
		e := s.Err()
		if e == nil {
			e = io.EOF
		}
		out <- incoming{Err: e}
	}()
	return out
}
func send(f *os.File, kind string, seq int, id Identity, w *WorkerResult, r *receipt) error {
	b, e := json.Marshal(message{Version: 1, Kind: kind, Seq: seq, Nonce: id.Nonce, Target: id, Worker: w, Receipt: r})
	if e != nil {
		return e
	}
	if len(b) > 4096 {
		return ErrProtocol
	}
	_, e = f.Write(append(b, '\n'))
	return e
}

type logger struct {
	dir, actor string
	mu         sync.Mutex
	seq        int
	events     []Event
}

func (l *logger) event(kind string, pid int, detail string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	e := Event{l.actor, l.seq, time.Now().UnixNano(), kind, os.Getpid(), pid, detail}
	l.events = append(l.events, e)
	b, _ := json.Marshal(e)
	f, err := os.OpenFile(filepath.Join(l.dir, l.actor+".jsonl"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err == nil {
		_, _ = f.Write(append(b, '\n'))
		_ = f.Close()
	}
}
func pipe() (*os.File, *os.File, error) { return os.Pipe() }
func closeFiles(fs ...*os.File) {
	for _, f := range fs {
		if f != nil {
			_ = f.Close()
		}
	}
}
func life(f *os.File) <-chan struct{} {
	ch := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, f); close(ch) }()
	return ch
}
func canonical(p string) string {
	q, e := filepath.EvalSymlinks(p)
	if e != nil {
		return ""
	}
	q, _ = filepath.Abs(q)
	return q
}
func cleanRoot(p string) bool {
	if e := os.RemoveAll(p); e != nil {
		return false
	}
	_, e := os.Lstat(p)
	return os.IsNotExist(e)
}
func writeJSON(p string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	return os.WriteFile(p, b, 0600)
}
func readJob(p string) (jobConfig, error) {
	var c jobConfig
	fd, e := unix.Open(p, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if e != nil {
		return c, e
	}
	f := os.NewFile(uintptr(fd), "job")
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Size() > 65536 {
		return c, ErrProtocol
	}
	b, e := io.ReadAll(io.LimitReader(f, 65537))
	if e != nil {
		return c, e
	}
	if e = strictJSON(b, &c, 65536); e != nil {
		return c, e
	}
	if validateRequest(Config{TempRoot: filepath.Dir(c.Work)}, c.Request) != nil || len(c.Nonce) != 32 || !filepath.IsAbs(c.Work) || !filepath.IsAbs(c.Evidence) {
		return c, ErrProtocol
	}
	return c, nil
}
func fdIdentity(f *os.File) FDIdentity {
	var st unix.Stat_t
	if unix.Fstat(int(f.Fd()), &st) != nil {
		return FDIdentity{}
	}
	return FDIdentity{uint64(st.Dev), st.Ino, uint32(st.Mode)}
}
func waitAbsent(m *identityMonitor, pid int, d time.Duration) bool {
	until := time.Now().Add(d)
	for {
		if m.absent(pid) {
			return true
		}
		if time.Now().After(until) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
}
func killIdentity(m *identityMonitor, id Identity) error {
	if m.absent(id.PID) {
		return fmt.Errorf("%w: already absent", errIdentityUnavailable)
	}
	v, e := m.identity(id.PID, id.Nonce)
	if e = classifyIdentity(id, v, e); e != nil {
		return e
	}
	if e = syscall.Kill(id.PID, syscall.SIGKILL); e == syscall.ESRCH {
		return fmt.Errorf("%w: exited before signal", errIdentityUnavailable)
	}
	return e
}
func signalReceipt(m *identityMonitor, r *receipt, l *logger) {
	l.event("target_kill", r.Target.PID, r.StopReason)
	e := killIdentity(m, r.Target)
	if errors.Is(e, errIdentityUnavailable) {
		r.SignalSkipped = true
		r.SignalSkipReason = e.Error()
		l.event("signal_skipped", r.Target.PID, e.Error())
	} else if e != nil {
		r.Error = e.Error()
	}
}
func settleReceipt(m *identityMonitor, r *receipt, ownWait bool) {
	r.Absent = waitAbsent(m, r.Target.PID, 2*time.Second)
	if r.SignalSkipped && r.Absent && r.Error == "" {
		r.SignalResolvedBy = "registered_absence"
		if ownWait && r.Waited {
			r.SignalResolvedBy = "own_wait_and_absence"
		}
	}
	if r.SignalSkipped && r.SignalResolvedBy == "" && r.Error == "" {
		r.Error = "unavailable identity without verified termination"
	}
}

func (s *Supervisor) run(ctx context.Context, q Request) (result Result, runErr error) {
	start := time.Now()
	result = Result{State: Preparing, HostPID: os.Getpid(), Worker: WorkerResult{ExitCode: -1, Mode: q.Mode}}
	defer func() {
		result.Wall = time.Since(start)
		if runErr != nil {
			result.Error = runErr.Error()
			if result.State != Cancelled {
				result.State = Failed
			}
		}
	}()
	m, e := monitor()
	if e != nil {
		return result, e
	}
	exe, e := os.Executable()
	if e != nil {
		return result, e
	}
	exe = canonical(exe)
	if exe == "" {
		return result, ErrProtocol
	}
	work, e := os.MkdirTemp(s.config.TempRoot, "lantern-supervisor-")
	if e != nil {
		return result, e
	}
	result.WorkRoot = work
	defer func() {
		result.WorkRootClean = cleanRoot(work)
		if !result.WorkRootClean {
			runErr = errors.Join(runErr, fmt.Errorf("work root cleanup failed"))
			result.State = Failed
		}
	}()
	evidenceParent := s.config.EvidenceRoot
	if evidenceParent == "" {
		evidenceParent = s.config.TempRoot
	}
	evidence, e := os.MkdirTemp(evidenceParent, "supervisor-evidence-")
	if e != nil {
		return result, e
	}
	result.EvidenceDir = evidence
	if s.config.EvidenceRoot == "" {
		defer os.RemoveAll(evidence)
	}
	var nonce [16]byte
	if _, e = rand.Read(nonce[:]); e != nil {
		return result, e
	}
	result.Nonce = hex.EncodeToString(nonce[:])
	l := logger{dir: evidence, actor: "host"}
	l.event("host_started", 0, "")
	defer func() { result.Events = append([]Event(nil), l.events...) }()
	var owned []*os.File
	defer func() { closeFiles(owned...) }()
	makePipe := func() (*os.File, *os.File, error) {
		r, w, e := pipe()
		if e == nil {
			owned = append(owned, r, w)
		}
		return r, w, e
	}
	lifeR, lifeW, e := makePipe()
	if e != nil {
		return result, e
	}
	regR, regW, e := makePipe()
	if e != nil {
		return result, e
	}
	goR, goW, e := makePipe()
	if e != nil {
		return result, e
	}
	cmdR, cmdW, e := makePipe()
	if e != nil {
		return result, e
	}
	ackR, ackW, e := makePipe()
	if e != nil {
		return result, e
	}
	for _, f := range owned {
		result.ControlFDs = append(result.ControlFDs, fdIdentity(f))
	}
	c := jobConfig{Nonce: result.Nonce, Request: q, Work: work, Evidence: evidence, RetainEvidence: s.config.EvidenceRoot != "", ControlFDs: result.ControlFDs}
	configPath := filepath.Join(evidence, "job.json")
	if e = writeJSON(configPath, c); e != nil {
		return result, e
	}
	dev, e := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if e != nil {
		return result, e
	}
	defer dev.Close()
	p, e := spawnFixed(exe, []string{helperArgument, "guardian", configPath}, []*os.File{dev, dev, dev, lifeR, regW, goR, cmdR, ackW}, false)
	if e != nil {
		return result, e
	}
	result.GuardianPID = p.Pid
	l.event("guardian_started", p.Pid, "")
	closeFiles(lifeR, regW, goR, cmdR, ackW)
	// lifeW is held exclusively by this host Run. Helpers receive readers only;
	// the kernel closes it on real host exit, including SIGKILL.
	_ = lifeW
	type waitResult struct {
		state *os.ProcessState
		err   error
	}
	done := make(chan waitResult, 1)
	go func() { v, e := p.Wait(); done <- waitResult{v, e} }()
	prepared := messages(regR, result.Nonce)
	acks := messages(ackR, result.Nonce)
	registered, armed, goSent, preparedClosed, stopped := false, false, false, false, false
	bootstrapUnavailable := false
	var bootstrapConflict error
	var completed *receipt
	deadline := time.NewTimer(7 * time.Second)
	defer deadline.Stop()
	ctxDone := ctx.Done()
	commandSeq := 0
	stop := func(reason string) {
		if !stopped {
			stopped = true
			result.StopReason = reason
			l.event("cancel_requested", result.Task.PID, reason)
			if registered {
				commandSeq++
				_ = send(cmdW, "CANCEL", commandSeq, result.Task, nil, nil)
			} else {
				_ = lifeW.Close()
			}
		}
	}
	observe := func(v message) error {
		if v.Kind != "BOOTSTRAP" || result.Task.PID != 0 {
			return ErrProtocol
		}
		id := v.Target
		if id.PID <= 0 || id.StartSeconds == 0 || id.StartMicroseconds >= 1000000 || id.Image != exe {
			return ErrProtocol
		}
		// Keep the valid private-pipe claim even if libproc catches the launcher
		// in an exit transition. This is a candidate identity, never acceptance
		// of PREPARED and never permission to signal or authorize GO.
		result.Task = id
		l.event("launcher_observed", id.PID, "direct bootstrap identity; GO remains gated")
		actual, e := m.identity(id.PID, result.Nonce)
		if e != nil {
			bootstrapUnavailable = true
			result.SignalSkipReason = fmt.Errorf("%w: bootstrap: %v", errIdentityUnavailable, e).Error()
			l.event("identity_unavailable", id.PID, result.SignalSkipReason)
			return nil
		}
		if e = classifyIdentity(id, actual, nil); e != nil {
			bootstrapConflict = e
			return e
		}
		if actual.Image != exe {
			bootstrapConflict = fmt.Errorf("%w: bootstrap image mismatch; no signal sent", ErrProtocol)
			return bootstrapConflict
		}
		return nil
	}
	accept := func(v message) error {
		if bootstrapConflict != nil {
			return bootstrapConflict
		}
		if registered || v.Kind != "PREPARED" || (result.Task.PID != 0 && result.Task != v.Target) {
			return ErrProtocol
		}
		actual, e := m.identity(v.Target.PID, result.Nonce)
		if e != nil {
			return e
		}
		if classifyIdentity(v.Target, actual, nil) != nil || v.Target.Image != exe || actual.Image != exe {
			return ErrProtocol
		}
		result.Task = v.Target
		bootstrapUnavailable = false
		result.SignalSkipReason = ""
		registered = true
		result.Registered = true
		result.State = StateRegistered
		l.event("registered", v.Target.PID, "")
		return nil
	}
	for {
		select {
		case msg, ok := <-prepared:
			if !ok || msg.Err != nil {
				prepared = nil
				preparedClosed = true
				if msg.Err != nil && msg.Err != io.EOF {
					runErr = msg.Err
					_ = p.Kill()
				}
				continue
			}
			if msg.Message.Kind == "BOOTSTRAP" {
				if e = observe(msg.Message); e != nil {
					runErr = e
					_ = p.Kill()
				}
				continue
			}
			if e = accept(msg.Message); e != nil {
				runErr = e
				_ = p.Kill()
				continue
			}
			if q.Gate == Registered {
				l.event("fault_window", result.Task.PID, string(q.Gate))
			} else {
				commandSeq++
				if e = send(cmdW, "REGISTERED", commandSeq, result.Task, nil, nil); e != nil {
					runErr = e
					_ = p.Kill()
				}
			}
		case msg, ok := <-acks:
			if !ok || msg.Err != nil {
				acks = nil
				if msg.Err != nil && msg.Err != io.EOF {
					runErr = msg.Err
					_ = p.Kill()
				}
				continue
			}
			v := msg.Message
			if registered && v.Target != result.Task {
				runErr = ErrProtocol
				_ = p.Kill()
				continue
			}
			switch v.Kind {
			case "ARMED":
				if !registered || armed {
					runErr = ErrProtocol
					_ = p.Kill()
					continue
				}
				armed = true
				result.Armed = true
				result.State = StateArmed
				l.event("armed", result.Task.PID, "acknowledged")
				if q.Gate == Armed {
					l.event("fault_window", result.Task.PID, string(q.Gate))
				} else {
					goSent = true
					result.GoAuthorized = true
					result.State = Running
					l.event("go_authorized", result.Task.PID, "")
					if e = send(goW, "GO", 1, result.Task, nil, nil); e != nil {
						runErr = e
						_ = p.Kill()
					}
				}
			case "ENTERED":
				if !goSent || v.Worker == nil || v.Worker.PID != result.Task.PID {
					runErr = ErrProtocol
					_ = p.Kill()
					continue
				}
				result.Worker = *v.Worker
				l.event("payload_entered", result.Task.PID, "")
			case "COMPLETE":
				if v.Receipt == nil {
					runErr = ErrProtocol
					_ = p.Kill()
					continue
				}
				completed = v.Receipt
				result.State = Draining
			default:
				runErr = ErrProtocol
				_ = p.Kill()
			}
		case wr := <-done:
			result.HostWaitedGuardian = wr.err == nil && wr.state != nil
			l.event("guardian_waited", p.Pid, "own direct child")
			// Pipes drain before interpreting peer loss: a successful child can
			// exit before the host goroutine observes its final live receipt.
			if completed == nil && acks != nil {
				timer := time.NewTimer(time.Second)
			drain:
				for {
					select {
					case msg, ok := <-acks:
						if !ok || msg.Err != nil {
							break drain
						}
						v := msg.Message
						if v.Kind == "ENTERED" && v.Worker != nil {
							result.Worker = *v.Worker
						}
						if v.Kind == "COMPLETE" {
							completed = v.Receipt
							break drain
						}
					case <-timer.C:
						break drain
					}
				}
				timer.Stop()
			}
			if completed != nil && completed.Error == "" && completed.Waited && completed.Absent && completed.Clean && wr.state != nil && wr.state.Success() {
				result.Task = completed.Target
				result.Worker = completed.Worker
				result.ControlFDs = append(result.ControlFDs, completed.Controls...)
				result.GuardianWaitedTask = true
				result.TaskAbsent = true
				result.StopReason = completed.StopReason
				result.SignalSkipped = completed.SignalSkipped
				result.SignalSkipReason = completed.SignalSkipReason
				result.SignalResolvedBy = completed.SignalResolvedBy
				if !goSent && runErr == nil && ctx.Err() == nil {
					runErr = fmt.Errorf("%w: fixed helper completed before GO", ErrProtocol)
				}
			} else {
				l.event("peer_lost", p.Pid, "guardian exited without valid completion")
				if completed != nil {
					if completed.Error != "" {
						runErr = errors.Join(runErr, ErrPeerLost, fmt.Errorf("guardian completion: %s", completed.Error))
					}
					if !completed.Clean {
						runErr = errors.Join(runErr, ErrPeerLost, fmt.Errorf("guardian cleanup failed"))
					}
				}
				if !registered && !preparedClosed && prepared != nil {
					timer := time.NewTimer(time.Second)
					waiting := true
					for waiting {
						select {
						case msg, ok := <-prepared:
							if !ok || msg.Err != nil {
								waiting = false
								continue
							}
							if msg.Message.Kind == "BOOTSTRAP" {
								e = observe(msg.Message)
							} else if msg.Message.Kind == "PREPARED" {
								if result.Task.PID == 0 {
									e = observe(message{Kind: "BOOTSTRAP", Target: msg.Message.Target})
								}
								waiting = false
							} else {
								e = ErrProtocol
								waiting = false
							}
							if e != nil {
								runErr = e
								waiting = false
							}
						case <-timer.C:
							runErr = ErrProtocol
							waiting = false
						}
					}
					timer.Stop()
				}
				if result.Task.PID > 0 {
					rr := receipt{Target: result.Task, StopReason: "guardian_lost"}
					switch {
					case bootstrapConflict != nil:
						rr.Error = bootstrapConflict.Error()
						rr.SignalSkipped = true
						rr.SignalSkipReason = rr.Error
						l.event("signal_skipped", rr.Target.PID, rr.Error)
					case bootstrapUnavailable && !registered:
						rr.SignalSkipped = true
						rr.SignalSkipReason = result.SignalSkipReason
						l.event("signal_skipped", rr.Target.PID, rr.SignalSkipReason)
					default:
						signalReceipt(m, &rr, &l)
					}
					settleReceipt(m, &rr, false)
					if rr.SignalResolvedBy != "" && !registered {
						rr.SignalResolvedBy = "bootstrap_pid_absence"
					}
					result.TaskAbsent = rr.Absent
					result.SignalSkipped = rr.SignalSkipped
					result.SignalSkipReason = rr.SignalSkipReason
					result.SignalResolvedBy = rr.SignalResolvedBy
					if rr.Error != "" {
						runErr = errors.Join(runErr, errors.New(rr.Error))
					}
				} else {
					// Only the fixed pre-spawn gate proves there was no task. EOF
					// with an unknown identity after possible spawn proves neither
					// its PID nor disappearance.
					result.TaskAbsent = q.Gate == BeforeSpawn
				}
				if runErr == nil {
					runErr = ErrPeerLost
				}
				result.StopReason = "guardian_lost"
			}
			result.Registered = registered
			result.Armed = armed
			result.GoAuthorized = goSent
			result.GuardianAbsent = waitAbsent(m, p.Pid, time.Second)
			if !result.TaskAbsent || !result.GuardianAbsent {
				runErr = errors.Join(runErr, fmt.Errorf("task or guardian disappearance unverified"))
			}
			if ctx.Err() != nil {
				result.State = Cancelled
				if runErr != nil {
					return result, errors.Join(ctx.Err(), runErr)
				}
				return result, ctx.Err()
			}
			if runErr != nil {
				result.State = Failed
				return result, runErr
			}
			result.State = Completed
			return result, nil
		case <-ctxDone:
			stop("cancel")
			ctxDone = nil
		case <-deadline.C:
			runErr = fmt.Errorf("supervision wall deadline")
			stop("wall_limit")
			_ = p.Kill()
		}
	}
}

func helperMain(role, path string) int {
	c, e := readJob(path)
	if e != nil {
		return 125
	}
	if role == "guardian" {
		return guardianMain(c, path)
	}
	return launcherMain(c)
}
func launcherMain(c jobConfig) int {
	l := logger{dir: c.Evidence, actor: "launcher"}
	m, e := monitor()
	if e != nil {
		return 125
	}
	reg, goR, aR, appR, permit := os.NewFile(3, "prepared"), os.NewFile(4, "go"), os.NewFile(5, "guardian-life"), os.NewFile(6, "host-life"), os.NewFile(7, "permit")
	defer closeFiles(reg, goR, aR, appR, permit)
	aGone, appGone := life(aR), life(appR)
	id, e := m.identity(os.Getpid(), c.Nonce)
	if e != nil {
		return 125
	}
	if send(reg, "BOOTSTRAP", 1, id, nil, nil) != nil {
		return 125
	}
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	permission := make(chan bool, 1)
	go func() { var b [1]byte; n, e := permit.Read(b[:]); permission <- n == 1 && e == nil && b[0] == 1 }()
	select {
	case ok := <-permission:
		if !ok {
			return 125
		}
	case <-aGone:
		return 125
	case <-appGone:
		return 125
	case <-timeout.C:
		return 125
	}
	l.event("prepared", id.PID, "")
	if send(reg, "PREPARED", 2, id, nil, nil) != nil {
		return 125
	}
	frames := messages(goR, c.Nonce)
	select {
	case v := <-frames:
		if v.Err != nil || v.Message.Kind != "GO" || v.Message.Target != id {
			return 125
		}
	case <-aGone:
		return 125
	case <-appGone:
		return 125
	case <-timeout.C:
		return 125
	}
	select {
	case <-aGone:
		return 125
	case <-appGone:
		return 125
	default:
	}
	l.event("go_received", id.PID, "")
	exe, e := os.Executable()
	if e != nil {
		return 125
	}
	_, e = spawnFixed(canonical(exe), []string{helperArgument, "worker", string(c.Request.Mode)}, []*os.File{os.Stdin, os.Stdout, os.Stderr}, true)
	if e != nil {
		l.event("exec_error", id.PID, e.Error())
		fmt.Fprintln(os.Stderr, e)
	}
	return 125
}

func guardianMain(c jobConfig, path string) int {
	l := logger{dir: c.Evidence, actor: "guardian"}
	m, e := monitor()
	if e != nil {
		return 125
	}
	appR, regW, goR, cmdR, ackW := os.NewFile(3, "host-life"), os.NewFile(4, "prepared-transfer"), os.NewFile(5, "go-transfer"), os.NewFile(6, "commands"), os.NewFile(7, "ack")
	defer closeFiles(appR, regW, goR, cmdR, ackW)
	appGone := life(appR)
	r := receipt{Target: Identity{Nonce: c.Nonce}, Worker: WorkerResult{ExitCode: -1, Mode: c.Request.Mode}}
	ackSeq := 0
	finish := func() int {
		settleReceipt(m, &r, true)
		if c.Request.Gate == NoGate && !r.Worker.Entered && r.StopReason == "" && r.Error == "" {
			r.Error = "fixed worker did not enter"
		}
		r.Clean = cleanRoot(c.Work)
		l.event("cleanup", r.Target.PID, "")
		_ = writeJSON(filepath.Join(c.Evidence, "guardian-receipt.json"), r)
		ackSeq++
		_ = send(ackW, "COMPLETE", ackSeq, r.Target, nil, &r)
		l.event("complete", r.Target.PID, "")
		if !c.RetainEvidence {
			_ = os.RemoveAll(c.Evidence)
		}
		if r.Error != "" || !r.Clean || !r.Absent {
			return 1
		}
		return 0
	}
	if c.Request.Gate == BeforeSpawn {
		l.event("fault_window", 0, string(BeforeSpawn))
		select {
		case <-appGone:
			r.StopReason = "app_eof"
			return finish()
		case <-time.After(5 * time.Second):
			r.Error = "startup fault not injected"
			return finish()
		}
	}
	aR, aW, e := pipe()
	if e != nil {
		r.Error = e.Error()
		return finish()
	}
	defer closeFiles(aR, aW)
	permitR, permitW, e := pipe()
	if e != nil {
		r.Error = e.Error()
		return finish()
	}
	defer closeFiles(permitR, permitW)
	outR, outW, e := pipe()
	if e != nil {
		r.Error = e.Error()
		return finish()
	}
	defer closeFiles(outR, outW)
	inR, inW, e := pipe()
	if e != nil {
		r.Error = e.Error()
		return finish()
	}
	defer closeFiles(inR, inW)
	dev, e := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if e != nil {
		r.Error = e.Error()
		return finish()
	}
	defer dev.Close()
	exe, e := os.Executable()
	if e != nil {
		r.Error = e.Error()
		return finish()
	}
	p, e := spawnFixed(canonical(exe), []string{helperArgument, "launcher", path}, []*os.File{inR, outW, dev, regW, goR, aR, appR, permitR}, false)
	if e != nil {
		r.Error = e.Error()
		return finish()
	}
	r.Controls = append(r.Controls, c.ControlFDs...)
	for _, f := range []*os.File{appR, regW, goR, cmdR, ackW, aR, aW, permitR, permitW} {
		r.Controls = append(r.Controls, fdIdentity(f))
	}
	closeFiles(inR, outW, aR, regW, goR, permitR)
	r.Target, e = m.identity(p.Pid, c.Nonce)
	if e != nil {
		_ = p.Kill()
		_, _ = p.Wait()
		r.Error = e.Error()
		return finish()
	}
	l.event("launcher_spawned", p.Pid, "")
	type waited struct {
		state *os.ProcessState
		err   error
	}
	done := make(chan waited, 1)
	go func() { s, e := p.Wait(); done <- waited{s, e} }()
	if c.Request.Gate == AfterSpawn {
		l.event("fault_window", p.Pid, string(AfterSpawn))
	} else {
		_, _ = permitW.Write([]byte{1})
		_ = permitW.Close()
	}
	commands := messages(cmdR, c.Nonce)
	markers := make(chan struct {
		v   WorkerResult
		err error
	}, 1)
	go func() {
		var v WorkerResult
		s := bufio.NewScanner(outR)
		s.Buffer(make([]byte, 512), 4097)
		var e error
		if !s.Scan() {
			e = s.Err()
			if e == nil {
				e = io.EOF
			}
		} else {
			e = strictJSON(s.Bytes(), &v, 4096)
		}
		markers <- struct {
			v   WorkerResult
			err error
		}{v, e}
	}()
	// Worker announces entry immediately; use a scanner, not EOF, so a held
	// worker can be observed before cancellation or a peer fault is injected.
	// The single marker is bounded; further stdout is not a command channel.
	_ = markers
	wall := time.NewTimer(5 * time.Second)
	defer wall.Stop()
	stopped, armed := false, false
	stop := func(reason string) {
		if !stopped {
			stopped = true
			r.StopReason = reason
			if c.Request.Mode == ExitRace {
				_, _ = inW.Write([]byte{1})
			}
			signalReceipt(m, &r, &l)
		}
	}
	markerSeen := false
	acceptMarker := func(v WorkerResult) {
		if !v.Entered || v.PID != r.Target.PID || v.Mode != c.Request.Mode {
			r.Error = "invalid worker marker"
			stop("protocol_error")
			return
		}
		for _, actual := range v.OpenFDs {
			for _, control := range r.Controls {
				if actual.Device == control.Device && actual.Inode == control.Inode && actual.Mode == control.Mode {
					v.ControlFDsInherited = true
				}
			}
		}
		if v.ControlFDsInherited {
			r.Error = "worker inherited supervision control endpoint"
			stop("protocol_error")
		}
		r.Worker = v
		markerSeen = true
		ackSeq++
		_ = send(ackW, "ENTERED", ackSeq, r.Target, &r.Worker, nil)
		l.event("payload_entered", p.Pid, "")
	}
	for {
		select {
		case marker := <-markers:
			markers = nil
			if marker.err == nil {
				acceptMarker(marker.v)
			}
		case wr := <-done:
			r.Waited = wr.err == nil && wr.state != nil
			if !markerSeen && markers != nil {
				select {
				case marker := <-markers:
					markers = nil
					if marker.err == nil {
						acceptMarker(marker.v)
					}
				case <-time.After(100 * time.Millisecond):
				}
			}
			if wr.state != nil {
				r.Worker.ExitCode = wr.state.ExitCode()
				if ws, ok := wr.state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
					r.Worker.Signal = int(ws.Signal())
				}
			}
			l.event("target_waited", p.Pid, strconv.Itoa(r.Worker.ExitCode))
			return finish()
		case <-appGone:
			stop("app_eof")
			appGone = nil
		case v, ok := <-commands:
			if !ok {
				commands = nil
				continue
			}
			if v.Err != nil {
				stop("command_lost")
				commands = nil
				continue
			}
			if v.Message.Target != r.Target {
				r.Error = "command identity mismatch"
				stop("protocol_error")
				continue
			}
			switch v.Message.Kind {
			case "REGISTERED":
				if armed {
					r.Error = "duplicate arm"
					stop("protocol_error")
					continue
				}
				armed = true
				l.event("armed", p.Pid, "")
				ackSeq++
				_ = send(ackW, "ARMED", ackSeq, r.Target, nil, nil)
			case "CANCEL":
				stop("cancel")
			default:
				r.Error = "unknown command"
				stop("protocol_error")
			}
		case <-wall.C:
			stop("wall_limit")
		}
	}
}

func workerMain(mode Mode) int {
	if validateRequest(Config{TempRoot: "/tmp"}, Request{Kind: GoLifecycle, Mode: mode}) != nil {
		return 125
	}
	r := WorkerResult{Entered: true, PID: os.Getpid(), Mode: mode, ExitCode: -1, OriginalGroup: syscall.Getpgrp()}
	if mode == SetSID {
		if _, e := syscall.Setsid(); e != nil {
			return 125
		}
	}
	if mode == SetPGID {
		if e := syscall.Setpgid(0, 0); e != nil {
			return 125
		}
	}
	r.Group = syscall.Getpgrp()
	r.Session, _ = unix.Getsid(0)
	if names, e := os.ReadDir("/dev/fd"); e == nil {
		for _, n := range names {
			fd, e := strconv.Atoi(n.Name())
			if e != nil || fd < 3 {
				continue
			}
			var st unix.Stat_t
			if unix.Fstat(fd, &st) == nil {
				r.OpenFDs = append(r.OpenFDs, FDIdentity{uint64(st.Dev), st.Ino, uint32(st.Mode)})
			}
		}
	}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		r.EnvKeys = append(r.EnvKeys, key)
	}
	if e := json.NewEncoder(os.Stdout).Encode(r); e != nil {
		return 125
	}
	switch mode {
	case Normal:
		time.Sleep(300 * time.Millisecond)
		return 0
	case ExpectedError:
		return 1
	case ExitRace:
		var b [1]byte
		_, _ = os.Stdin.Read(b[:])
		return 0
	default:
		time.Sleep(4 * time.Second)
		return 0
	}
}
