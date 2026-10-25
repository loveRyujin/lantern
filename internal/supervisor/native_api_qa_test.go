//go:build darwin

package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type qaSupervisorCall struct {
	Request Request           `json:"request"`
	Result  Result            `json:"result"`
	Error   string            `json:"error,omitempty"`
	Raw     map[string]string `json:"raw_events"`
}

type qaSupervisorProof struct {
	Name         string             `json:"name"`
	Status       string             `json:"status"`
	Source       string             `json:"source_commit"`
	Dirty        bool               `json:"source_dirty"`
	HostPID      int                `json:"actual_caller_pid"`
	Calls        []qaSupervisorCall `json:"calls"`
	Observations map[string]any     `json:"observations"`
}

func qaSupervisorNativeProof(t *testing.T) *qaSupervisorProof {
	t.Helper()
	if os.Getenv("DARWIN_SUPERVISOR_REAL_QA") != "1" {
		t.Skip("requires explicit Darwin supervisor API acceptance gate")
	}
	out := os.Getenv("DARWIN_SUPERVISOR_QA_OUT")
	if !filepath.IsAbs(out) || filepath.Clean(out) != out {
		t.Fatal("DARWIN_SUPERVISOR_QA_OUT must be an absolute clean evidence directory")
	}
	if err := os.MkdirAll(out, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(out, t.Name()+".json")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal("refuse to overwrite prior independent API evidence: ", err)
	}
	command := func(args ...string) string {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		b, err := exec.CommandContext(ctx, "git", args...).Output()
		if err != nil {
			t.Fatalf("actual source metadata unavailable: %v", err)
		}
		return strings.TrimSpace(string(b))
	}
	p := &qaSupervisorProof{Name: t.Name(), Source: command("rev-parse", "HEAD"), Dirty: command("status", "--porcelain") != "", HostPID: os.Getpid(), Observations: map[string]any{}}
	t.Cleanup(func() {
		p.Status = "passed"
		if t.Failed() {
			p.Status = "failed"
		}
		if err := json.NewEncoder(file).Encode(p); err != nil {
			t.Error("cannot persist actual API acceptance evidence: ", err)
		}
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	})
	return p
}

func qaSupervisorNative(t *testing.T) (*Supervisor, Config) {
	t.Helper()
	base := t.TempDir()
	c := Config{TempRoot: filepath.Join(base, "work"), EvidenceRoot: filepath.Join(base, "evidence")}
	for _, path := range []string{c.TempRoot, c.EvidenceRoot} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	return New(c), c
}

func qaSupervisorRecord(p *qaSupervisorProof, q Request, r Result, err error) {
	call := qaSupervisorCall{Request: q, Result: r, Raw: map[string]string{}}
	if err != nil {
		call.Error = err.Error()
	}
	for _, actor := range []string{"host", "guardian", "launcher"} {
		b, err := os.ReadFile(filepath.Join(r.EvidenceDir, actor+".jsonl"))
		if err == nil {
			call.Raw[actor] = string(b)
		}
	}
	p.Calls = append(p.Calls, call)
}

func qaSupervisorTerminal(t *testing.T, c Config, r Result) {
	t.Helper()
	if r.HostPID != os.Getpid() || r.GuardianPID <= 0 || r.GuardianPID == r.HostPID || r.Task.PID <= 0 || r.Task.PID == r.HostPID || r.Task.PID == r.GuardianPID || r.Worker.PID != r.Task.PID || !r.Worker.Entered || !r.Registered || !r.Armed || !r.GoAuthorized {
		t.Fatalf("Run did not execute in the actual caller and hand off the same task identity: %+v", r)
	}
	if len(r.Nonce) != 32 || r.Task.Nonce != r.Nonce || r.Task.StartSeconds == 0 || r.Task.StartMicroseconds >= 1000000 || !filepath.IsAbs(r.Task.Image) {
		t.Fatalf("actual registered identity missing: %+v", r.Task)
	}
	if !r.HostWaitedGuardian || !r.GuardianWaitedTask || !r.GuardianAbsent || !r.TaskAbsent || !r.WorkRootClean || r.Worker.ControlFDsInherited {
		t.Fatalf("Run returned before own Wait, actual disappearance or cleanup: %+v", r)
	}
	for _, pid := range []int{r.GuardianPID, r.Task.PID} {
		if err := unix.Kill(pid, 0); !errors.Is(err, unix.ESRCH) {
			t.Fatalf("terminal API result claimed absence but PID %d is observable: %v", pid, err)
		}
	}
	if r.WorkRoot == "" || filepath.Dir(r.WorkRoot) != c.TempRoot {
		t.Fatalf("work directory is not a new child owned by this call: %q", r.WorkRoot)
	}
	if _, err := os.Lstat(r.WorkRoot); !os.IsNotExist(err) {
		t.Fatalf("returned work cleanup is not real: %q %v", r.WorkRoot, err)
	}
	if r.EvidenceDir == "" || filepath.Dir(r.EvidenceDir) != c.EvidenceRoot {
		t.Fatalf("retained evidence is not separately owned: %q", r.EvidenceDir)
	}
	if info, err := os.Stat(r.EvidenceDir); err != nil || !info.IsDir() {
		t.Fatalf("retained evidence disappeared with work: %v", err)
	}
	seq := 0
	for _, e := range r.Events {
		if e.Actor != "host" || e.PID != os.Getpid() || e.Seq != seq+1 {
			t.Fatalf("caller events originate from a wrapper or mix another call: %+v", e)
		}
		seq = e.Seq
	}
}

type qaSupervisorAsync struct {
	r   Result
	err error
}

func qaSupervisorStart(t *testing.T, s *Supervisor, parent context.Context, q Request) <-chan qaSupervisorAsync {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	done, finished := make(chan qaSupervisorAsync, 1), make(chan struct{})
	go func() {
		defer close(finished)
		r, err := s.Run(ctx, q)
		done <- qaSupervisorAsync{r, err}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(8 * time.Second):
			t.Error("API call did not drain after test cancellation; fixture cleanup cannot count as Run cleanup")
		}
	})
	return done
}

func qaSupervisorWaitEntry(t *testing.T, evidence string, count int, exclude map[string]bool, pending ...<-chan qaSupervisorAsync) []int {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		seen := map[int]bool{}
		paths, _ := filepath.Glob(filepath.Join(evidence, "supervisor-evidence-*", "host.jsonl"))
		for _, path := range paths {
			if exclude[filepath.Dir(path)] {
				continue
			}
			b, _ := os.ReadFile(path)
			for _, line := range strings.Split(string(b), "\n") {
				var e Event
				if json.Unmarshal([]byte(line), &e) == nil && e.Actor == "host" && e.Kind == "payload_entered" && e.TargetPID > 0 {
					seen[e.TargetPID] = true
				}
			}
		}
		if len(seen) >= count {
			pids := make([]int, 0, len(seen))
			for pid := range seen {
				pids = append(pids, pid)
			}
			return pids
		}
		for _, ch := range pending {
			select {
			case r := <-ch:
				t.Fatalf("Run returned before its real entry checkpoint: state=%s error=%v worker=%+v", r.r.State, r.err, r.r.Worker)
			default:
			}
		}
		select {
		case <-deadline.C:
			t.Fatal("required live payload entry was not observed; this cannot count as cancellation coverage")
		case <-tick.C:
		}
	}
}

func qaSupervisorLimits(t *testing.T) map[int]unix.Rlimit {
	t.Helper()
	out := map[int]unix.Rlimit{}
	for _, resource := range []int{unix.RLIMIT_AS, unix.RLIMIT_CORE, unix.RLIMIT_CPU, unix.RLIMIT_DATA, unix.RLIMIT_FSIZE, unix.RLIMIT_NOFILE, unix.RLIMIT_NPROC, unix.RLIMIT_STACK} {
		var limit unix.Rlimit
		if err := unix.Getrlimit(resource, &limit); err != nil {
			t.Fatal(err)
		}
		out[resource] = limit
	}
	return out
}

func qaSupervisorAwaitHeldLauncher(t *testing.T, evidence string, done <-chan qaSupervisorAsync) (Identity, *identityMonitor) {
	t.Helper()
	m, err := monitor()
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	until := time.NewTimer(2 * time.Second)
	defer until.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		paths, _ := filepath.Glob(filepath.Join(evidence, "supervisor-evidence-*"))
		for _, dir := range paths {
			pid, held := 0, false
			for _, actor := range []string{"host", "guardian"} {
				b, _ := os.ReadFile(filepath.Join(dir, actor+".jsonl"))
				for _, line := range strings.Split(string(b), "\n") {
					var event Event
					if json.Unmarshal([]byte(line), &event) != nil {
						continue
					}
					if event.Actor == "host" && event.PID == os.Getpid() && event.Kind == "launcher_observed" {
						pid = event.TargetPID
					}
					held = held || event.Actor == "guardian" && event.Kind == "fault_window" && event.Detail == string(AfterSpawn)
				}
			}
			if pid > 0 && held {
				job, err := readJob(filepath.Join(dir, "job.json"))
				if err != nil || job.Request.Gate != AfterSpawn {
					t.Fatal("owned gate metadata was not readable for independent fault injection")
				}
				id, err := m.identity(pid, job.Nonce)
				if err != nil || id.Image != canonical(exe) || id.PID == os.Getpid() {
					t.Fatalf("held launcher was not the owned sealed child: %+v %v", id, err)
				}
				return id, m
			}
		}
		select {
		case early := <-done:
			t.Fatalf("Run returned before the explicit after-spawn gate: %+v %v", early.r, early.err)
		case <-until.C:
			t.Fatal("held launcher checkpoint was not reached; sleep is not an injection oracle")
		case <-tick.C:
		}
	}
}

func TestSupervisorQANativeResultsRecoveryAndHostInvariants(t *testing.T) {
	p := qaSupervisorNativeProof(t)
	s, c := qaSupervisorNative(t)
	env := os.Environ()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	limits := qaSupervisorLimits(t)
	nonces, roots := map[string]bool{}, map[string]bool{}
	// Force a real preparation failure without relying on permissions or a
	// missing platform. Work may have been allocated, but no actor may start.
	badEvidence := filepath.Join(filepath.Dir(c.EvidenceRoot), "owned-regular-file")
	prior := []byte("preserve this owned file on preparation failure")
	if err := os.WriteFile(badEvidence, prior, 0600); err != nil {
		t.Fatal(err)
	}
	badConfig := c
	badConfig.EvidenceRoot = badEvidence
	q := Request{Kind: GoLifecycle, Mode: Normal}
	failed, failedErr := New(badConfig).Run(t.Context(), q)
	qaSupervisorRecord(p, q, failed, failedErr)
	if failedErr == nil || failed.State != Failed || failed.GuardianPID != 0 || failed.Task.PID != 0 || failed.Worker.Entered || failed.GoAuthorized || failed.WorkRoot == "" || !failed.WorkRootClean {
		t.Fatalf("preparation failure was not terminal or left owned work: state=%s error=%v result=%+v", failed.State, failedErr, failed)
	}
	if _, err := os.Lstat(failed.WorkRoot); !os.IsNotExist(err) {
		t.Fatalf("preparation failure did not actually remove its work directory: %v", err)
	}
	if got, err := os.ReadFile(badEvidence); err != nil || string(got) != string(prior) {
		t.Fatalf("preparation failure damaged existing evidence path: %q %v", got, err)
	}
	gateCtx, gateCancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer gateCancel()
	gateRequest := Request{Kind: GoLifecycle, Mode: Hold, Gate: AfterSpawn}
	gateDone := qaSupervisorStart(t, s, gateCtx, gateRequest)
	launcher, m := qaSupervisorAwaitHeldLauncher(t, c.EvidenceRoot, gateDone)
	current, identityErr := m.identity(launcher.PID, launcher.Nonce)
	if identityErr != nil || current != launcher {
		t.Fatal("owned held-launcher identity changed before injected startup exit")
	}
	if err := unix.Kill(launcher.PID, unix.SIGKILL); err != nil {
		t.Fatal(err)
	}
	gateResult := <-gateDone
	qaSupervisorRecord(p, gateRequest, gateResult.r, gateResult.err)
	gr := gateResult.r
	if !errors.Is(gateResult.err, ErrProtocol) || gr.State != Failed || gr.GoAuthorized || gr.Worker.Entered || !gr.HostWaitedGuardian || !gr.GuardianWaitedTask || !gr.TaskAbsent || !gr.GuardianAbsent || !gr.WorkRootClean || gr.Task.PID != launcher.PID {
		t.Fatalf("pre-GO launcher exit was mistaken for execution success: %+v error=%v", gr, gateResult.err)
	}
	if _, err := os.Lstat(gr.WorkRoot); !os.IsNotExist(err) {
		t.Fatal("failed pre-GO execution returned without actual work cleanup")
	}
	roots[gr.WorkRoot], roots[gr.EvidenceDir], nonces[gr.Nonce] = true, true, true
	p.Observations["pre_go_exit_failed_without_delivery"] = true
	for _, mode := range []Mode{Normal, ExpectedError, Normal, Hold, Normal} {
		ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
		defer cancel()
		q := Request{Kind: GoLifecycle, Mode: mode}
		var r Result
		var callErr error
		if mode == Hold {
			done := qaSupervisorStart(t, s, ctx, q)
			// Ignore earlier completed calls' retained diagnostics. Only a new
			// directory's actual live entry can synchronize this cancellation.
			qaSupervisorWaitEntry(t, c.EvidenceRoot, 1, roots, done)
			cancel()
			got := <-done
			r, callErr = got.r, got.err
			if !errors.Is(callErr, context.Canceled) || r.State != Cancelled || r.StopReason != "cancel" || r.Worker.ExitCode != -1 || r.Worker.Signal != int(unix.SIGKILL) {
				t.Fatalf("cancel was not a real terminal cancellation: state=%s error=%v worker=%+v", r.State, callErr, r.Worker)
			}
		} else {
			r, callErr = s.Run(ctx, q)
			if callErr != nil || r.State != Completed {
				t.Fatalf("supervision did not complete for fixed %s worker: state=%s error=%v", mode, r.State, callErr)
			}
			want := 0
			if mode == ExpectedError {
				want = 1
			}
			if r.Worker.ExitCode != want || r.Worker.Signal != 0 || r.StopReason != "" {
				t.Fatalf("supervisor completion hid or changed the worker outcome for %s: %+v", mode, r)
			}
		}
		cancel()
		qaSupervisorRecord(p, q, r, callErr)
		qaSupervisorTerminal(t, c, r)
		if nonces[r.Nonce] || roots[r.WorkRoot] || roots[r.EvidenceDir] {
			t.Fatal("consecutive calls reused nonce or per-call directories")
		}
		nonces[r.Nonce], roots[r.WorkRoot], roots[r.EvidenceDir] = true, true, true
	}
	if after, err := os.Getwd(); err != nil || after != cwd || !reflect.DeepEqual(env, os.Environ()) || !reflect.DeepEqual(limits, qaSupervisorLimits(t)) {
		t.Fatal("Run changed the actual host cwd, environment or rlimits")
	}
	p.Observations["host_configuration_unchanged"] = true
	p.Observations["normal_after_error_and_cancel"] = true
}

func TestSupervisorQANativeConcurrentIsolationAndLateCancel(t *testing.T) {
	p := qaSupervisorNativeProof(t)
	s, c := qaSupervisorNative(t)
	aCtx, cancelA := context.WithTimeout(t.Context(), 8*time.Second)
	bCtx, cancelB := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancelA()
	defer cancelB()
	q := Request{Kind: GoLifecycle, Mode: Hold}
	aDone, bDone := qaSupervisorStart(t, s, aCtx, q), qaSupervisorStart(t, s, bCtx, q)
	pids := qaSupervisorWaitEntry(t, c.EvidenceRoot, 2, nil, aDone, bDone)
	cancelA()
	a := <-aDone
	qaSupervisorRecord(p, q, a.r, a.err)
	if !errors.Is(a.err, context.Canceled) || a.r.State != Cancelled || a.r.Worker.ExitCode != -1 {
		t.Fatalf("first independent context did not cancel its own task: %+v %v", a.r, a.err)
	}
	qaSupervisorTerminal(t, c, a.r)
	otherPID := 0
	for _, pid := range pids {
		if pid != a.r.Task.PID {
			otherPID = pid
		}
	}
	if otherPID <= 0 || unix.Kill(otherPID, 0) != nil {
		t.Fatal("cancelling A terminated the other registered task")
	}
	select {
	case b := <-bDone:
		t.Fatalf("B ended before independent A cancellation was checked: %+v %v", b.r, b.err)
	default:
	}
	aSnapshot, _ := json.Marshal(a.r)
	cancelA() // A has terminated. This must have no further lifecycle effect.
	b := <-bDone
	qaSupervisorRecord(p, q, b.r, b.err)
	if b.err != nil || b.r.State != Completed || b.r.Worker.ExitCode != 0 || b.r.Task.PID != otherPID {
		t.Fatalf("A cancellation or late cancellation contaminated B's natural completion: %+v %v", b.r, b.err)
	}
	qaSupervisorTerminal(t, c, b.r)
	if a.r.Nonce == b.r.Nonce || a.r.WorkRoot == b.r.WorkRoot || a.r.EvidenceDir == b.r.EvidenceDir || a.r.Task.PID == b.r.Task.PID || a.r.GuardianPID == b.r.GuardianPID || a.r.HostPID != b.r.HostPID {
		t.Fatal("two simultaneous Runs did not preserve independent identities and directories in the actual same caller")
	}
	if after, _ := json.Marshal(a.r); string(after) != string(aSnapshot) {
		t.Fatal("another Run mutated A's already-returned result")
	}
	p.Observations["other_task_alive_after_cancel"] = otherPID
	p.Observations["late_cancel_has_no_effect"] = true
}

func TestSupervisorQANativeCredentialFDAndEnvironment(t *testing.T) {
	p := qaSupervisorNativeProof(t)
	s, c := qaSupervisorNative(t)
	t.Setenv("LANTERN_QA_HOST_ONLY_TOKEN", "owned-dummy-no-secret")
	hostEnv := os.Environ()
	f, err := os.CreateTemp(t.TempDir(), "private-owned-credential-sentinel-")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fd, err := unix.FcntlInt(f.Fd(), unix.F_DUPFD, 200)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if _, err = unix.FcntlInt(uintptr(fd), unix.F_SETFD, 0); err != nil {
		t.Fatal(err)
	}
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC != 0 {
		t.Fatal("host positive control did not actually expose a non-CLOEXEC descriptor")
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	q := Request{Kind: GoLifecycle, Mode: Normal}
	r, err := s.Run(ctx, q)
	qaSupervisorRecord(p, q, r, err)
	if err != nil || r.State != Completed || r.Worker.ExitCode != 0 {
		t.Fatalf("actual API execution missing: %+v %v", r, err)
	}
	qaSupervisorTerminal(t, c, r)
	for _, inherited := range r.Worker.OpenFDs {
		if inherited.Device == uint64(st.Dev) && inherited.Inode == st.Ino {
			t.Fatal("actual worker inherited the host's private non-CLOEXEC credential descriptor")
		}
	}
	for _, inherited := range r.Worker.OpenFDs {
		for _, control := range r.ControlFDs {
			if inherited.Device == control.Device && inherited.Inode == control.Inode {
				t.Fatal("actual worker inherited a lifecycle control endpoint")
			}
		}
	}
	var after unix.Stat_t
	afterFlags, flagErr := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if unix.Fstat(fd, &after) != nil || after.Ino != st.Ino || after.Dev != st.Dev || flagErr != nil || afterFlags != flags {
		t.Fatal("Run closed or changed flags of an existing host credential descriptor")
	}
	if !reflect.DeepEqual(hostEnv, os.Environ()) {
		t.Fatal("Run changed the actual host environment")
	}
	keys := map[string]bool{}
	for _, key := range r.Worker.EnvKeys {
		keys[key] = true
	}
	if keys["LANTERN_QA_HOST_ONLY_TOKEN"] || !keys["PATH"] || !keys["LANG"] {
		t.Fatalf("worker did not receive the actual minimal allowlisted environment: %v", r.Worker.EnvKeys)
	}
	p.Observations["credential_fd"] = fd
	p.Observations["credential_identity"] = FDIdentity{uint64(st.Dev), st.Ino, uint32(st.Mode)}
	p.Observations["host_non_cloexec_unchanged"] = true
	p.Observations["credential_not_inherited"] = true
}
