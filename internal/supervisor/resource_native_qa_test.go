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
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

func qaResourceNativeProof(t *testing.T) *qaSupervisorProof {
	t.Helper()
	if os.Getenv("DARWIN_RESOURCE_REAL_QA") != "1" {
		t.Skip("requires explicit native resource acceptance gate")
	}
	out := os.Getenv("DARWIN_RESOURCE_QA_OUT")
	if !filepath.IsAbs(out) || filepath.Clean(out) != out {
		t.Fatal("DARWIN_RESOURCE_QA_OUT must be absolute and clean")
	}
	if err := os.MkdirAll(out, 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(out, t.Name()+".json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		b, err := exec.Command("git", args...).Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(b))
	}
	p := &qaSupervisorProof{Name: t.Name(), Source: git("rev-parse", "HEAD"), Dirty: git("status", "--porcelain") != "", HostPID: os.Getpid(), Observations: map[string]any{}}
	t.Cleanup(func() {
		p.Status = "passed"
		if t.Failed() {
			p.Status = "failed"
		}
		if err := json.NewEncoder(f).Encode(p); err != nil {
			t.Error(err)
		}
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})
	return p
}

func qaResourceTerminal(t *testing.T, cfg Config, r Result) {
	t.Helper()
	qaSupervisorTerminal(t, cfg, r)
	u := r.Usage
	// A live cancellation can complete before the first 10ms target tick.
	// Preserve that blind window rather than fabricating a running sample or
	// delaying cancellation merely to satisfy a test.
	minimum := 2
	if r.State == Cancelled && r.StopReason == "cancel" {
		minimum = 1
	}
	if !u.MonitoringReady || !u.MonitoringStarted || len(u.Samples) < minimum || len(u.Samples) > MaxResourceSamples || u.GoMonotonicNS == 0 || u.WaitMonotonicNS < u.GoMonotonicNS || u.ReceiptMonotonicNS < u.WaitMonotonicNS || u.Wall != time.Duration(u.WaitMonotonicNS-u.GoMonotonicNS) {
		t.Fatalf("actual resource monitoring or guardian own-Wait interval absent: %+v", u)
	}
	if len(u.Samples) == 1 && (u.Samples[0].Phase != "pre-go" || !u.Samples[0].Valid || u.SuccessfulSamples != 0 || u.CPU != 0 || u.PeakRSSBytes != 0 || u.FirstSampleMonotonicNS != 0 || u.LastSampleMonotonicNS != 0) {
		t.Fatal("cancellation without a running sample fabricated active usage")
	}
	if r.StopReason == "cpu_limit" || r.StopReason == "rss_limit" {
		observed := false
		for _, sample := range u.Samples {
			if sample.Valid && sample.Phase == "running" && sample.MonotonicNS >= u.GoMonotonicNS && sample.MonotonicNS <= u.TriggerMonotonicNS {
				observed = observed || r.StopReason == "cpu_limit" && sample.CPU > r.Budget.CPU || r.StopReason == "rss_limit" && sample.RSSBytes > r.Budget.RSSBytes
			}
		}
		if !observed {
			t.Fatal("resource stop has no real running threshold observation")
		}
	}
	for _, sample := range u.Samples {
		if sample.PID != r.Task.PID {
			t.Fatal("sample belongs to another call")
		}
	}
}

func TestResourceQANativeRecoveryAndCancellation(t *testing.T) {
	p := qaResourceNativeProof(t)
	limits, environment := qaSupervisorLimits(t), os.Environ()
	cwd, cwdErr := os.Getwd()
	if cwdErr != nil {
		t.Fatal(cwdErr)
	}
	s, cfg := qaSupervisorNative(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	q := Request{Kind: GoResource, Mode: CPUBusy, Budget: ResourceBudget{CPU: 250 * time.Millisecond, RSSBytes: 256 << 20, Wall: 5 * time.Second}}
	r, err := s.Run(ctx, q)
	qaSupervisorRecord(p, q, r, err)
	qaResourceTerminal(t, cfg, r)
	if !errors.Is(err, ErrCPULimit) || r.State != Failed || r.StopReason != "cpu_limit" || r.Usage.TriggerReason != "cpu_limit" {
		t.Fatalf("real CPU stop missing: %v %+v", err, r)
	}
	q = Request{Kind: GoResource, Mode: Normal}
	r, err = s.Run(ctx, q)
	qaSupervisorRecord(p, q, r, err)
	qaResourceTerminal(t, cfg, r)
	if err != nil || r.State != Completed || r.Worker.ExitCode != 0 {
		t.Fatalf("post-limit recovery failed: %v %+v", err, r)
	}
	prior := map[string]bool{}
	for _, call := range p.Calls {
		prior[call.Result.EvidenceDir] = true
	}
	callCtx, callCancel := context.WithCancel(ctx)
	q = Request{Kind: GoResource, Mode: LowCPUSleep, Budget: ResourceBudget{CPU: 3 * time.Second, RSSBytes: 256 << 20, Wall: 5 * time.Second}}
	done := qaSupervisorStart(t, s, callCtx, q)
	qaSupervisorWaitEntry(t, cfg.EvidenceRoot, 1, prior, done)
	callCancel()
	a := <-done
	qaSupervisorRecord(p, q, a.r, a.err)
	qaResourceTerminal(t, cfg, a.r)
	if !errors.Is(a.err, context.Canceled) || errors.Is(a.err, ErrWallLimit) || a.r.State != Cancelled || a.r.StopReason != "cancel" {
		t.Fatalf("explicit live cancellation became a resource timeout: %v %+v", a.err, a.r)
	}
	q = Request{Kind: GoResource, Mode: Normal}
	r, err = s.Run(ctx, q)
	qaSupervisorRecord(p, q, r, err)
	qaResourceTerminal(t, cfg, r)
	if err != nil || r.Worker.ExitCode != 0 {
		t.Fatalf("post-cancel recovery failed: %v", err)
	}
	// Cancellation after a completed blocking call cannot alter retained usage.
	before, _ := json.Marshal(r)
	callCancel()
	after, _ := json.Marshal(r)
	if string(before) != string(after) {
		t.Fatal("late cancellation mutated a returned value")
	}
	p.Observations["live_entry_before_cancel"] = true
	afterCWD, err := os.Getwd()
	if err != nil || afterCWD != cwd || !reflect.DeepEqual(environment, os.Environ()) || !reflect.DeepEqual(limits, qaSupervisorLimits(t)) {
		t.Fatal("resource Run polluted the real host cwd/environment/rlimits")
	}
	p.Observations["host_cwd_environment_rlimits_unchanged"] = true
}

func TestResourceQANativeConcurrentIsolation(t *testing.T) {
	p := qaResourceNativeProof(t)
	s, cfg := qaSupervisorNative(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	sleepCtx, sleepCancel := context.WithCancel(ctx)
	defer sleepCancel()
	sleepQ := Request{Kind: GoResource, Mode: LowCPUSleep, Budget: ResourceBudget{CPU: 3 * time.Second, RSSBytes: 256 << 20, Wall: 5 * time.Second}}
	cpuQ := Request{Kind: GoResource, Mode: CPUBusy, Budget: ResourceBudget{CPU: time.Second, RSSBytes: 256 << 20, Wall: 5 * time.Second}}
	sleepDone := qaSupervisorStart(t, s, sleepCtx, sleepQ)
	cpuDone := qaSupervisorStart(t, s, ctx, cpuQ)
	qaSupervisorWaitEntry(t, cfg.EvidenceRoot, 2, nil, sleepDone, cpuDone)
	sleepCancel()
	a, b := <-sleepDone, <-cpuDone
	qaSupervisorRecord(p, sleepQ, a.r, a.err)
	qaSupervisorRecord(p, cpuQ, b.r, b.err)
	qaResourceTerminal(t, cfg, a.r)
	qaResourceTerminal(t, cfg, b.r)
	if !errors.Is(a.err, context.Canceled) || !errors.Is(b.err, ErrCPULimit) || errors.Is(b.err, context.Canceled) || b.r.StopReason != "cpu_limit" {
		t.Fatalf("concurrent stop reasons crossed: %v / %v", a.err, b.err)
	}
	if a.r.Nonce == b.r.Nonce || a.r.Task.PID == b.r.Task.PID || a.r.WorkRoot == b.r.WorkRoot || a.r.EvidenceDir == b.r.EvidenceDir {
		t.Fatal("concurrent calls shared identity or directories")
	}
	p.Observations["independent_cancel_and_cpu_stop"] = true
}

func TestResourceQANativeMonitorReadFailure(t *testing.T) {
	p := qaResourceNativeProof(t)
	s, cfg := qaSupervisorNative(t)
	m, err := monitor()
	if err != nil {
		t.Fatal(err)
	}
	original := m.info
	var inject atomic.Bool
	var injectedReads atomic.Int32
	// Test-owned telemetry fault only: real PID/birth/path checks and actual
	// processes remain live. This is not evidence of an actual kernel outage.
	m.info = func(pid, flavor int32, arg uint64, ptr unsafe.Pointer, n int32) int32 {
		if flavor == 4 && inject.Load() {
			// Two isolated failures recover on the next actual kernel read,
			// followed by a continuous failure. A stale failureStart must not
			// let the earlier transient bursts shorten the final 50ms window.
			attempt := injectedReads.Add(1)
			if attempt != 2 && attempt != 4 {
				return 0
			}
		}
		return original(pid, flavor, arg, ptr, n)
	}
	// Register before qaSupervisorStart's cleanup: its cancellation/drain runs
	// first if an assertion aborts, before this shared function is restored.
	t.Cleanup(func() { m.info = original })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	q := Request{Kind: GoResource, Mode: LowCPUSleep, Budget: ResourceBudget{CPU: 3 * time.Second, RSSBytes: 256 << 20, Wall: 5 * time.Second}}
	done := qaSupervisorStart(t, s, ctx, q)
	qaSupervisorWaitEntry(t, cfg.EvidenceRoot, 1, nil, done)
	inject.Store(true)
	completed := <-done
	r, err := completed.r, completed.err
	qaSupervisorRecord(p, q, r, err)
	qaResourceTerminal(t, cfg, r)
	if !errors.Is(err, ErrMonitor) || errors.Is(err, ErrWallLimit) || r.State != Failed || r.StopReason != "monitor_error" {
		t.Fatalf("continuous unavailable telemetry did not fail closed: %v %+v", err, r)
	}
	var first, last uint64
	recoveries := 0
	for _, sample := range r.Usage.Samples {
		if sample.Phase == "running" && sample.Valid && first != 0 {
			recoveries++
			first, last = 0, 0
		}
		if sample.Phase == "running" && !sample.Valid {
			if sample.Error == "" {
				t.Fatal("failed telemetry masqueraded as valid zero")
			}
			if first == 0 {
				first = sample.MonotonicNS
			}
			last = sample.MonotonicNS
		}
	}
	if recoveries != 2 || first == 0 || last-first < uint64(ResourceReadFailureWindow) || r.Usage.TriggerMonotonicNS < first+uint64(ResourceReadFailureWindow) {
		t.Fatal("monitor failure was not measured from the first continuous failed read")
	}
	p.Observations["injected_fault"] = "PROC_PIDTASKINFO short read after actual payload_entered: fail/success/fail/success/continuous fail; identity probes unmodified"
	p.Observations["same_run_recoveries"] = recoveries
	p.Observations["real_run_wait_cleanup_after_injection"] = true
	m.info = original
	q = Request{Kind: GoResource, Mode: Normal}
	r, err = s.Run(ctx, q)
	qaSupervisorRecord(p, q, r, err)
	qaResourceTerminal(t, cfg, r)
	if err != nil || r.Worker.ExitCode != 0 {
		t.Fatalf("monitor-fault recovery failed: %v", err)
	}
}
