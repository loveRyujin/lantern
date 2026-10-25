//go:build darwin

package main

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

//go:embed supervision_payload.py
var supervisionPayload []byte

type supervisionConfig struct {
	ID      string     `json:"id"`
	Nonce   string     `json:"nonce"`
	Window  string     `json:"window"`
	Fault   string     `json:"fault"`
	Mode    string     `json:"mode"`
	Control string     `json:"control"`
	Exe     string     `json:"exe"`
	Task    taskConfig `json:"task"`
}
type supervisionLog struct {
	dir, actor string
	seq        int
}

func (l *supervisionLog) event(kind string, pid int, detail string) {
	l.seq++
	e := SupervisionEvent{Actor: l.actor, Seq: l.seq, AtNS: time.Now().UnixNano(), Kind: kind, PID: os.Getpid(), TargetPID: pid, Detail: detail}
	b, _ := json.Marshal(e)
	f, err := os.OpenFile(filepath.Join(l.dir, l.actor+".jsonl"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err == nil {
		_, _ = f.Write(append(b, '\n'))
		_ = f.Close()
	}
}

type supervisionRead struct {
	Frame supervisionFrame
	Err   error
}

func supervisionFrames(f *os.File, nonce string) <-chan supervisionRead {
	out := make(chan supervisionRead, 8)
	go func() {
		defer close(out)
		s := bufio.NewScanner(f)
		s.Buffer(make([]byte, 512), 4097)
		seq := 0
		for s.Scan() {
			seq++
			v, e := decodeSupervisionFrame(s.Bytes(), nonce, seq)
			out <- supervisionRead{v, e}
			if e != nil {
				return
			}
		}
		e := s.Err()
		if e == nil {
			e = io.EOF
		}
		out <- supervisionRead{Err: e}
	}()
	return out
}
func supervisionSend(f *os.File, kind string, seq int, id supervisionIdentity) error {
	return json.NewEncoder(f).Encode(supervisionFrame{Kind: kind, Nonce: id.Nonce, Seq: seq, PID: id.PID, StartSeconds: id.StartSeconds, StartMicroseconds: id.StartMicroseconds, Image: id.Image})
}
func supervisionIdentityNow(m *processMonitor, pid int, nonce string) (supervisionIdentity, error) {
	v, e := m.identity(pid)
	if e != nil {
		return supervisionIdentity{}, e
	}
	return supervisionIdentity{pid, v.StartSeconds, v.StartMicroseconds, m.path(pid), nonce}, nil
}
func supervisionIdentityFrom(f supervisionFrame) supervisionIdentity {
	return supervisionIdentity{f.PID, f.StartSeconds, f.StartMicroseconds, f.Image, f.Nonce}
}
func supervisionSame(m *processMonitor, id supervisionIdentity) bool {
	v, e := m.identity(id.PID)
	return e == nil && v.StartSeconds == id.StartSeconds && v.StartMicroseconds == id.StartMicroseconds
}
func supervisionKill(m *processMonitor, id supervisionIdentity) error {
	if id.PID <= 0 {
		return nil
	}
	if processAbsent(m, id.PID) {
		return fmt.Errorf("%w: already absent", errSupervisionIdentityUnavailable)
	}
	actual, e := m.identity(id.PID)
	if e = classifySupervisionIdentity(id, supervisionIdentity{PID: actual.PID, StartSeconds: actual.StartSeconds, StartMicroseconds: actual.StartMicroseconds}, e); e != nil {
		return e
	}
	if e = syscall.Kill(id.PID, syscall.SIGKILL); e == syscall.ESRCH {
		return fmt.Errorf("%w: exited before signal", errSupervisionIdentityUnavailable)
	}
	return e
}
func supervisionSignalResult(r *supervisionReceipt, log *supervisionLog, e error) {
	if e == nil {
		return
	}
	if errors.Is(e, errSupervisionIdentityUnavailable) {
		r.SignalSkipped = true
		r.SignalSkipReason = e.Error()
		log.event("signal_skipped", r.Target.PID, e.Error())
		return
	}
	if r.Error == "" {
		r.Error = e.Error()
	} else {
		r.Error += "; " + e.Error()
	}
}
func supervisionResolveSkipped(r *supervisionReceipt, log *supervisionLog, registered bool) {
	if !r.SignalSkipped {
		return
	}
	if r.TargetAbsent && ((r.Actor == "guardian" && r.Waited && r.WaitedPID == r.Target.PID) || (r.Actor == "app" && registered)) {
		r.SignalResolvedBy = "registered_absence"
		if r.Actor == "guardian" {
			r.SignalResolvedBy = "own_wait_and_absence"
		}
		log.event("target_absent", r.Target.PID, "identity unavailable resolved only by "+r.SignalResolvedBy)
		return
	}
	if r.Error == "" {
		r.Error = "identity unavailable without verified termination"
	}
}
func supervisionLife(f *os.File) <-chan struct{} {
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, f); close(done) }()
	return done
}
func supervisionGone(m *processMonitor, pid int) bool {
	if pid <= 0 {
		return true
	}
	return waitAbsent(context.Background(), m, pid, 2*time.Second)
}
func supervisionPreserveMarker(c supervisionConfig) *supervisionMarker {
	var v supervisionMarker
	if readJSON(filepath.Join(c.Task.Out, "marker.json"), &v) == nil {
		_ = writeJSON(filepath.Join(c.Control, "payload-marker.json"), v)
		return &v
	}
	if readJSON(filepath.Join(c.Control, "payload-marker.json"), &v) == nil {
		return &v
	}
	return nil
}
func supervisionReceiptWrite(c supervisionConfig, r supervisionReceipt) {
	_ = writeJSON(filepath.Join(c.Control, r.Actor+"-receipt.json"), r)
}
func supervisionReadConfig(path string) (supervisionConfig, error) {
	var c supervisionConfig
	f, e := openConfig(path)
	if e != nil {
		return c, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, 65537))
	if e != nil || len(b) > 65536 {
		return c, fmt.Errorf("invalid bounded supervisor config")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e = d.Decode(&c); e != nil {
		return c, e
	}
	if e = d.Decode(new(interface{})); e != io.EOF {
		return c, fmt.Errorf("incomplete supervisor config")
	}
	b, e = os.ReadFile(c.Task.Script)
	if e != nil || !bytes.Equal(b, supervisionPayload) || c.Nonce == "" || !filepath.IsAbs(c.Control) || c.Task.Budget.WallMS > 5000 {
		return c, fmt.Errorf("invalid fixed supervisor task")
	}
	return c, nil
}
func supervisionHelper(kind, path string) int {
	c, e := supervisionReadConfig(path)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		return 125
	}
	switch kind {
	case "__supervision-app":
		return supervisionApp(c, path)
	case "__supervision-guardian":
		return supervisionGuardian(c, path)
	case "__supervision-launcher":
		return supervisionLauncher(c, path)
	}
	return 125
}

func supervisionLauncher(c supervisionConfig, path string) int {
	log := supervisionLog{c.Control, "launcher", 0}
	m, e := newProcessMonitor()
	if e != nil {
		return 125
	}
	reg, goRead, aRead, appRead, permit := os.NewFile(3, "prepared"), os.NewFile(4, "go"), os.NewFile(5, "guardian-life"), os.NewFile(6, "app-life"), os.NewFile(7, "registration-permit")
	defer reg.Close()
	defer goRead.Close()
	defer aRead.Close()
	defer appRead.Close()
	defer permit.Close()
	aGone, appGone := supervisionLife(aRead), supervisionLife(appRead)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	allowed := make(chan bool, 1)
	go func() { var b [1]byte; n, e := permit.Read(b[:]); allowed <- n == 1 && e == nil && b[0] == 1 }()
	select {
	case ok := <-allowed:
		if !ok {
			return 125
		}
	case <-aGone:
		return 125
	case <-appGone:
		return 125
	case <-ctx.Done():
		return 125
	}
	id, e := supervisionIdentityNow(m, os.Getpid(), c.Nonce)
	if e != nil {
		return 125
	}
	log.event("prepared", id.PID, "")
	if e = supervisionSend(reg, "PREPARED", 1, id); e != nil {
		return 125
	}
	frames := supervisionFrames(goRead, c.Nonce)
	select {
	case frame := <-frames:
		if frame.Err != nil || frame.Frame.Kind != "GO" || supervisionIdentityFrom(frame.Frame) != id {
			return 125
		}
	case <-aGone:
		return 125
	case <-appGone:
		return 125
	case <-ctx.Done():
		return 125
	}
	// A previously observed loss prevents exec. A fault concurrent with this
	// check is handled by the already-registered surviving observer.
	select {
	case <-aGone:
		return 125
	case <-appGone:
		return 125
	default:
	}
	log.event("go_received", id.PID, "")
	for _, l := range []struct {
		which int
		max   uint64
	}{{syscall.RLIMIT_CPU, 8}, {syscall.RLIMIT_NOFILE, 128}, {syscall.RLIMIT_FSIZE, 16 << 20}, {syscall.RLIMIT_CORE, 0}} {
		if e = syscall.Setrlimit(l.which, &syscall.Rlimit{Cur: l.max, Max: l.max}); e != nil {
			return 125
		}
	}
	for fd := 3; fd <= 7; fd++ {
		syscall.CloseOnExec(fd)
	}
	if e = os.Chdir(c.Task.Root); e != nil {
		return 125
	}
	args := []string{"/usr/bin/sandbox-exec", "-f", c.Task.Policy, c.Task.Python, "-I", "-B", c.Task.Script, filepath.Join(c.Task.Root, "input", "supervision-task.json")}
	if e = syscall.Exec(args[0], args, fixedEnv(c.Task)); e != nil {
		fmt.Fprintln(os.Stderr, e)
	}
	return 125
}

func supervisionGuardian(c supervisionConfig, path string) int {
	log := supervisionLog{c.Control, "guardian", 0}
	m, e := newProcessMonitor()
	if e != nil {
		return 125
	}
	appR, regW, goR, commandR, ackW := os.NewFile(3, "app-life"), os.NewFile(4, "prepared-transfer"), os.NewFile(5, "go-transfer"), os.NewFile(6, "commands"), os.NewFile(7, "ack")
	defer appR.Close()
	defer regW.Close()
	defer goR.Close()
	defer commandR.Close()
	defer ackW.Close()
	appGone := supervisionLife(appR)
	r := supervisionReceipt{Actor: "guardian", PID: os.Getpid(), ExitCode: -1}
	finish := func() int {
		if marker := supervisionPreserveMarker(c); marker != nil {
			log.event("payload_entered", marker.PID, "retained payload marker")
		}
		r.Cleanup = cleanupFixtureRoot(c.Task.Root) == nil
		r.TargetAbsent = supervisionGone(m, r.Target.PID)
		supervisionResolveSkipped(&r, &log, r.Registered)
		log.event("cleanup", r.Target.PID, "")
		supervisionReceiptWrite(c, r)
		log.event("complete", r.Target.PID, "")
		if r.Error != "" || !r.Cleanup || !r.TargetAbsent {
			return 1
		}
		return 0
	}
	if c.Window == "before-spawn" {
		log.event("fault_window", 0, "before-spawn")
		select {
		case <-appGone:
			r.StopReason = "app_eof"
			return finish()
		case <-time.After(5 * time.Second):
			r.Error = "startup fault not injected"
			return finish()
		}
	}
	aR, aW, e := os.Pipe()
	if e != nil {
		r.Error = e.Error()
		return finish()
	}
	defer aW.Close()
	permitR, permitW, e := os.Pipe()
	if e != nil {
		aR.Close()
		r.Error = e.Error()
		return finish()
	}
	defer permitW.Close()
	cmd := exec.Command(c.Exe, "__supervision-launcher", path)
	cmd.Env = fixedEnv(c.Task)
	cmd.ExtraFiles = []*os.File{regW, goR, aR, appR, permitR}
	stdout := &boundedBuffer{limit: 64 << 10, overflow: make(chan struct{})}
	stderr := &boundedBuffer{limit: 64 << 10, overflow: make(chan struct{})}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if e = cmd.Start(); e != nil {
		aR.Close()
		permitR.Close()
		r.Error = e.Error()
		return finish()
	}
	aR.Close()
	permitR.Close()
	regW.Close()
	goR.Close()
	r.Target, e = supervisionIdentityNow(m, cmd.Process.Pid, c.Nonce)
	if e != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		r.Error = e.Error()
		return finish()
	}
	log.event("launcher_spawned", r.Target.PID, "")
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	if c.Window == "after-spawn" {
		log.event("fault_window", r.Target.PID, "after-spawn")
	} else {
		_, _ = permitW.Write([]byte{1})
		permitW.Close()
	}
	commands := supervisionFrames(commandR, c.Nonce)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	wall := time.NewTimer(5 * time.Second)
	defer wall.Stop()
	stopped := false
	stop := func(reason string) {
		if !stopped {
			stopped = true
			r.StopReason = reason
			log.event("target_kill", r.Target.PID, reason)
			supervisionSignalResult(&r, &log, supervisionKill(m, r.Target))
		}
	}
	for {
		select {
		case <-done:
			r.Waited = true
			r.WaitedPID = r.Target.PID
			r.ExitCode = cmd.ProcessState.ExitCode()
			log.event("target_waited", r.Target.PID, fmt.Sprint(r.ExitCode))
			return finish()
		case <-appGone:
			stop("app_eof")
			appGone = nil
		case msg, ok := <-commands:
			if !ok {
				commands = nil
				continue
			}
			if msg.Err != nil {
				stop("command_lost")
				commands = nil
				continue
			}
			if supervisionIdentityFrom(msg.Frame) != r.Target {
				r.Error = "command identity mismatch"
				stop("protocol_error")
				continue
			}
			switch msg.Frame.Kind {
			case "REGISTERED":
				if r.Armed {
					r.Error = "duplicate arm"
					stop("protocol_error")
					continue
				}
				r.Registered = true
				r.Armed = true
				log.event("armed", r.Target.PID, "")
				_ = supervisionSend(ackW, "ARMED", 1, r.Target)
			case "CANCEL":
				stop("cancel")
			default:
				r.Error = "unexpected guardian command"
				stop("protocol_error")
			}
		case <-stdout.overflow:
			stop("stdout_limit")
		case <-stderr.overflow:
			stop("stderr_limit")
		case <-wall.C:
			stop("wall_limit")
		case <-ticker.C:
			if info, e := m.sample(r.Target.PID); e == nil {
				if info.Resident > 512<<20 || m.cpuSeconds(info) > 5 {
					stop("resource_limit")
				}
			}
		}
	}
}

func supervisionApp(c supervisionConfig, path string) int {
	log := supervisionLog{c.Control, "app", 0}
	log.event("app_started", 0, "")
	m, e := newProcessMonitor()
	if e != nil {
		return 125
	}
	lifeR, lifeW, e := os.Pipe()
	if e != nil {
		return 125
	}
	defer lifeW.Close()
	regR, regW, e := os.Pipe()
	if e != nil {
		return 125
	}
	defer regR.Close()
	goR, goW, e := os.Pipe()
	if e != nil {
		return 125
	}
	defer goW.Close()
	cmdR, cmdW, e := os.Pipe()
	if e != nil {
		return 125
	}
	defer cmdW.Close()
	ackR, ackW, e := os.Pipe()
	if e != nil {
		return 125
	}
	defer ackR.Close()
	cmd := exec.Command(c.Exe, "__supervision-guardian", path)
	cmd.Env = fixedEnv(c.Task)
	cmd.ExtraFiles = []*os.File{lifeR, regW, goR, cmdR, ackW}
	if e = cmd.Start(); e != nil {
		return 125
	}
	lifeR.Close()
	regW.Close()
	goR.Close()
	cmdR.Close()
	ackW.Close()
	log.event("guardian_started", cmd.Process.Pid, "")
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	prepared := supervisionFrames(regR, c.Nonce)
	acks := supervisionFrames(ackR, c.Nonce)
	r := supervisionReceipt{Actor: "app", PID: os.Getpid(), ExitCode: -1}
	registered, armed, goSent, cancelSent, preparedClosed := false, false, false, false, false
	finish := func() int {
		if marker := supervisionPreserveMarker(c); marker != nil {
			log.event("payload_entered", marker.PID, "retained payload marker")
		}
		r.Registered = registered
		r.Armed = armed
		r.Go = goSent
		r.TargetAbsent = supervisionGone(m, r.Target.PID)
		supervisionResolveSkipped(&r, &log, registered)
		if !r.TargetAbsent {
			r.Error = "registered target remains"
		}
		r.Cleanup = cleanupFixtureRoot(c.Task.Root) == nil
		log.event("cleanup", r.Target.PID, "")
		supervisionReceiptWrite(c, r)
		log.event("complete", r.Target.PID, "")
		if r.Error != "" || !r.Cleanup {
			return 1
		}
		return 0
	}
	accept := func(f supervisionFrame) error {
		if registered || f.Kind != "PREPARED" {
			return fmt.Errorf("duplicate/unexpected prepared")
		}
		id := supervisionIdentityFrom(f)
		if !supervisionSame(m, id) || m.path(id.PID) != canonical(c.Exe) || id.Image != canonical(c.Exe) {
			return fmt.Errorf("launcher pre-exec identity mismatch")
		}
		r.Target = id
		registered = true
		log.event("registered", id.PID, "")
		return nil
	}
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	deadline := time.NewTimer(7 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case <-done:
			r.Waited = true
			r.WaitedPID = cmd.Process.Pid
			r.ExitCode = cmd.ProcessState.ExitCode()
			log.event("target_waited", cmd.Process.Pid, "own direct guardian child")
			var gr supervisionReceipt
			normal := readJSON(filepath.Join(c.Control, "guardian-receipt.json"), &gr) == nil && gr.Waited && gr.TargetAbsent && gr.Cleanup && gr.Error == "" && r.ExitCode == 0
			if normal {
				r.Target = gr.Target
				r.StopReason = gr.StopReason
				return finish()
			}
			log.event("peer_lost", cmd.Process.Pid, "guardian exited before valid completion")
			// If spawn raced guardian death, only the launcher's direct pipe can
			// resolve registration. No scan or harness PID is used by the mechanism.
			if r.Target.PID == 0 && !preparedClosed {
				timer := time.NewTimer(time.Second)
				waiting := true
				for waiting {
					select {
					case msg, ok := <-prepared:
						if !ok || msg.Err != nil {
							waiting = false
							preparedClosed = true
						} else {
							if e := accept(msg.Frame); e != nil {
								r.Error = e.Error()
							}
							waiting = false
						}
					case <-timer.C:
						r.Error = "unknown launcher registration did not close"
						waiting = false
					}
				}
				timer.Stop()
			}
			if r.Target.PID > 0 {
				log.event("target_kill", r.Target.PID, "guardian_lost")
				supervisionSignalResult(&r, &log, supervisionKill(m, r.Target))
			}
			r.StopReason = "guardian_lost"
			return finish()
		case msg, ok := <-prepared:
			if !ok {
				prepared = nil
				preparedClosed = true
				continue
			}
			if msg.Err != nil {
				prepared = nil
				preparedClosed = true
				if msg.Err != io.EOF {
					r.Error = msg.Err.Error()
					_ = cmd.Process.Kill()
				}
				continue
			}
			if e = accept(msg.Frame); e != nil {
				r.Error = e.Error()
				_ = cmd.Process.Kill()
				continue
			}
			if c.Window == "registered" {
				log.event("fault_window", r.Target.PID, "registered")
			} else {
				_ = supervisionSend(cmdW, "REGISTERED", 1, r.Target)
			}
		case msg, ok := <-acks:
			if !ok {
				acks = nil
				continue
			}
			if msg.Err != nil {
				acks = nil
				continue
			}
			if msg.Frame.Kind != "ARMED" || !registered || armed || supervisionIdentityFrom(msg.Frame) != r.Target {
				r.Error = "invalid armed acknowledgement"
				_ = cmd.Process.Kill()
				continue
			}
			armed = true
			log.event("armed", r.Target.PID, "acknowledged")
			if c.Window == "armed" {
				log.event("fault_window", r.Target.PID, "armed")
			} else {
				goSent = true
				log.event("go_authorized", r.Target.PID, "")
				if e = supervisionSend(goW, "GO", 1, r.Target); e != nil {
					r.Error = e.Error()
					_ = cmd.Process.Kill()
				}
			}
		case <-tick.C:
			if goSent && !cancelSent && (c.ID == "cancel" || c.ID == "exit-cancel-race") {
				marker := filepath.Join(c.Task.Out, "marker.json")
				if c.ID == "exit-cancel-race" {
					marker = filepath.Join(c.Task.Out, "exiting")
				}
				if _, e := os.Stat(marker); e == nil {
					cancelSent = true
					log.event("cancel_requested", r.Target.PID, "")
					if c.ID == "exit-cancel-race" {
						_ = os.WriteFile(filepath.Join(c.Task.Out, "exit-release"), []byte("release"), 0600)
					}
					_ = supervisionSend(cmdW, "CANCEL", 2, r.Target)
				}
			}
		case <-deadline.C:
			r.Error = "application supervision deadline"
			_ = cmd.Process.Kill()
		}
	}
}
