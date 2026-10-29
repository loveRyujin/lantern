package main

import (
	"strings"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/internal/supervisor"
)

// Synthetic observations test the artifact oracle only. They are never native
// measurement or enforcement evidence.
func qaResourceCase(id string) resourceAPICase {
	budget := supervisor.ResourceBudget{CPU: time.Second, RSSBytes: 128 << 20, Wall: 3 * time.Second}
	c := resourceAPICase{ID: id, WallMS: 450, Request: supervisor.Request{Kind: supervisor.GoResource, Mode: supervisor.Normal}, ErrorKinds: []string{}}
	mode, reason := supervisor.Normal, ""
	switch id {
	case "cpu-limit":
		mode, reason, budget = supervisor.CPUBusy, "cpu_limit", supervisor.ResourceBudget{CPU: 250 * time.Millisecond, RSSBytes: 256 << 20, Wall: 5 * time.Second}
	case "rss-limit":
		mode, reason, budget = supervisor.TouchMemory, "rss_limit", supervisor.ResourceBudget{CPU: 3 * time.Second, RSSBytes: 64 << 20, Wall: 5 * time.Second}
	case "wall-limit":
		mode, reason, budget = supervisor.LowCPUSleep, "wall_limit", supervisor.ResourceBudget{CPU: 3 * time.Second, RSSBytes: 256 << 20, Wall: 250 * time.Millisecond}
	case "multithread-cpu":
		mode, reason, budget = supervisor.MultiThreadCPU, "cpu_limit", supervisor.ResourceBudget{CPU: 2 * time.Second, RSSBytes: 256 << 20, Wall: 5 * time.Second}
	case "untouched-virtual":
		mode, budget = supervisor.UntouchedMemory, supervisor.ResourceBudget{CPU: 3 * time.Second, RSSBytes: 64 << 20, Wall: 5 * time.Second}
	}
	c.Request.Mode, c.ExpectedReason = mode, reason
	if id != "normal-defaults" {
		c.Request.Budget = budget
	}
	r := supervisor.Result{Budget: budget, HostPID: 1001, GuardianPID: 1002, Nonce: strings.Repeat("a", 32), Registered: true, Armed: true, GoAuthorized: true, State: supervisor.Completed, HostWaitedGuardian: true, GuardianWaitedTask: true, GuardianAbsent: true, TaskAbsent: true, WorkRootClean: true}
	r.Task = supervisor.Identity{PID: 1003, StartSeconds: 1700000000, StartMicroseconds: 123, Nonce: r.Nonce, Image: "/owned/sealed-resource-helper"}
	r.Worker = supervisor.WorkerResult{Entered: true, PID: r.Task.PID, Mode: mode}
	if mode == supervisor.TouchMemory {
		r.Worker.MappedBytes, r.Worker.TouchedBytes = 96<<20, 32<<20
	}
	if mode == supervisor.UntouchedMemory {
		r.Worker.MappedBytes = 256 << 20
	}
	const goNS = uint64(time.Second)
	makeSample := func(at uint64, cpu time.Duration, rss uint64, phase string) supervisor.ResourceSample {
		return supervisor.ResourceSample{PID: r.Task.PID, Phase: phase, MonotonicNS: at, Valid: true, RawMachUser: uint64(cpu) * 3 / 125, RawMachLiveThreadsUser: uint64(cpu) * 3 / 125, CPU: cpu, RSSBytes: rss, VirtualBytes: 1 << 30}
	}
	a := makeSample(goNS+uint64(50*time.Millisecond), 50*time.Millisecond, 16<<20, "running")
	b := makeSample(goNS+uint64(400*time.Millisecond), 100*time.Millisecond, 18<<20, "running")
	if reason == "cpu_limit" {
		b = makeSample(b.MonotonicNS, budget.CPU+50*time.Millisecond, b.RSSBytes, "running")
	}
	if mode == supervisor.MultiThreadCPU {
		b.RawMachLiveThreadsUser = uint64(b.CPU-200*time.Millisecond) * 3 / 125
	}
	if reason == "rss_limit" {
		b.RSSBytes = 80 << 20
	}
	u := supervisor.ResourceUsage{MonitoringReady: true, MonitoringStarted: true, TimebaseNumer: 125, TimebaseDenom: 3, GoMonotonicNS: goNS, WaitMonotonicNS: b.MonotonicNS + uint64(20*time.Millisecond), CPU: b.CPU, PeakRSSBytes: b.RSSBytes, PeakVirtualBytes: b.VirtualBytes, GoToFirstSample: time.Duration(a.MonotonicNS - goNS), MaxSampleGap: time.Duration(b.MonotonicNS - a.MonotonicNS), LastSampleToWait: 20 * time.Millisecond}
	u.Samples = []supervisor.ResourceSample{makeSample(goNS-1, 900*time.Millisecond, 40<<20, "pre-go"), a, b}
	u.ReceiptMonotonicNS = u.WaitMonotonicNS + uint64(10*time.Millisecond)
	u.Wall, u.ReceiptDelay = time.Duration(u.WaitMonotonicNS-u.GoMonotonicNS), 10*time.Millisecond
	if reason != "" {
		r.State, r.StopReason, r.Worker.ExitCode, r.Worker.Signal = supervisor.Failed, reason, -1, 9
		c.ErrorKinds, c.Error = []string{reason}, "observed typed resource error"
		u.TriggerReason, u.TriggerMonotonicNS = reason, b.MonotonicNS
		u.SignalAttemptMonotonicNS, u.SignalReturnMonotonicNS, u.SignalSent = b.MonotonicNS+1, b.MonotonicNS+2, true
		if reason == "cpu_limit" {
			u.CPUOvershoot = b.CPU - budget.CPU
		}
		if reason == "rss_limit" {
			u.RSSOvershootBytes = b.RSSBytes - budget.RSSBytes
		}
	}
	r.Usage = u
	c.Result = r
	return c
}

func qaResourceAllChecks(c resourceAPICase) bool {
	checks := resourceCaseChecks(c, 1001)
	if len(checks) == 0 {
		return false
	}
	for _, passed := range checks {
		if !passed {
			return false
		}
	}
	return true
}

func TestResourceAPIQAValidSyntheticOracleControls(t *testing.T) {
	for _, id := range []string{"normal-defaults", "cpu-limit", "rss-limit", "wall-limit", "multithread-cpu", "untouched-virtual"} {
		c := qaResourceCase(id)
		if !qaResourceAllChecks(c) {
			t.Fatalf("complete synthetic resource oracle control rejected: %s checks=%v", id, resourceCaseChecks(c, 1001))
		}
	}
}

func TestResourceAPIQARejectsFalseEnforcementAndAccounting(t *testing.T) {
	for name, test := range map[string]struct {
		id string
		fn func(*resourceAPICase)
	}{
		"normalized-config-is-not-installed": {"cpu-limit", func(c *resourceAPICase) { c.Result.Usage.MonitoringStarted = false }},
		"cpu-killed-by-context":              {"cpu-limit", func(c *resourceAPICase) { c.ErrorKinds = []string{"cancelled"}; c.Result.StopReason = "cancel" }},
		"cpu-naturally-exited": {"cpu-limit", func(c *resourceAPICase) {
			c.Result.State = supervisor.Completed
			c.Result.Worker.ExitCode = 0
			c.Result.Worker.Signal = 0
		}},
		"typed-cause-with-no-actual-error": {"cpu-limit", func(c *resourceAPICase) { c.Error = "" }},
		"resource-error-hides-protocol":    {"cpu-limit", func(c *resourceAPICase) { c.ErrorKinds = append(c.ErrorKinds, "protocol") }},
		"rss-peak-disagrees-with-raw":      {"rss-limit", func(c *resourceAPICase) { c.Result.Usage.PeakRSSBytes = 0 }},
		"virtual-peak-disagrees-with-raw":  {"rss-limit", func(c *resourceAPICase) { c.Result.Usage.PeakVirtualBytes = 0 }},
		"cpu-latest-became-historic-max":   {"normal-defaults", func(c *resourceAPICase) { c.Result.Usage.CPU = 900 * time.Millisecond }},
		"cpu-overshoot-fabricated":         {"cpu-limit", func(c *resourceAPICase) { c.Result.Usage.CPUOvershoot = 0 }},
		"rss-overshoot-fabricated":         {"rss-limit", func(c *resourceAPICase) { c.Result.Usage.RSSOvershootBytes = 0 }},
		"sample-gap-fabricated":            {"normal-defaults", func(c *resourceAPICase) { c.Result.Usage.MaxSampleGap = 0 }},
		"first-window-fabricated":          {"normal-defaults", func(c *resourceAPICase) { c.Result.Usage.GoToFirstSample = 0 }},
		"last-window-fabricated":           {"normal-defaults", func(c *resourceAPICase) { c.Result.Usage.LastSampleToWait = 0 }},
		"wrong-sample-pid":                 {"cpu-limit", func(c *resourceAPICase) { c.Result.Usage.Samples[1].PID++ }},
		"wrong-Mach-conversion":            {"cpu-limit", func(c *resourceAPICase) { c.Result.Usage.Samples[1].CPU++ }},
		"invalid-read-masquerades-as-zero": {"cpu-limit", func(c *resourceAPICase) {
			c.Result.Usage.Samples[1].Valid = false
			c.Result.Usage.Samples[1].Error = ""
		}},
		"different-worker-profile":        {"cpu-limit", func(c *resourceAPICase) { c.Result.Worker.Mode = supervisor.TouchMemory }},
		"wall-charged-through-receipt":    {"wall-limit", func(c *resourceAPICase) { c.Result.Usage.Wall += c.Result.Usage.ReceiptDelay }},
		"kill-after-actual-wait":          {"wall-limit", func(c *resourceAPICase) { c.Result.Usage.SignalAttemptMonotonicNS = c.Result.Usage.WaitMonotonicNS + 1 }},
		"untouched-map-was-never-created": {"untouched-virtual", func(c *resourceAPICase) { c.Result.Worker.MappedBytes = 0 }},
		"untouched-map-actually-touched":  {"untouched-virtual", func(c *resourceAPICase) { c.Result.Worker.TouchedBytes = 1 }},
		"retired-thread-cpu-not-observed": {"multithread-cpu", func(c *resourceAPICase) {
			for i := range c.Result.Usage.Samples {
				s := &c.Result.Usage.Samples[i]
				s.RawMachLiveThreadsUser = s.RawMachUser
				s.RawMachLiveThreadsSystem = s.RawMachSystem
			}
		}},
		"negative-case-duration": {"normal-defaults", func(c *resourceAPICase) { c.WallMS = -1 }},
	} {
		t.Run(name, func(t *testing.T) {
			c := qaResourceCase(test.id)
			if !qaResourceAllChecks(c) {
				t.Fatal("negative has no passing synthetic control")
			}
			test.fn(&c)
			if qaResourceAllChecks(c) {
				t.Fatal("contradictory resource observation was accepted")
			}
		})
	}
}
