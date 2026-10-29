package main

import (
	"context"
	"errors"
	"math/bits"
	"time"

	"github.com/loveRyujin/lantern/internal/supervisor"
)

const resourceAPISchema = "lantern.darwin-resource-api.v1"

type resourceAPISpec struct {
	ID         string             `json:"id"`
	Request    supervisor.Request `json:"request"`
	WantReason string             `json:"want_reason,omitempty"`
}

func resourceAPISpecs() []resourceAPISpec {
	makeSpec := func(id string, mode supervisor.Mode, cpu time.Duration, rss uint64, wall time.Duration, reason string) resourceAPISpec {
		return resourceAPISpec{id, supervisor.Request{Kind: supervisor.GoResource, Mode: mode, Budget: supervisor.ResourceBudget{CPU: cpu, RSSBytes: rss, Wall: wall}}, reason}
	}
	return []resourceAPISpec{
		makeSpec("normal-defaults", supervisor.Normal, 0, 0, 0, ""),
		makeSpec("cpu-limit", supervisor.CPUBusy, 250*time.Millisecond, 256<<20, 5*time.Second, "cpu_limit"),
		makeSpec("rss-limit", supervisor.TouchMemory, 3*time.Second, 64<<20, 5*time.Second, "rss_limit"),
		makeSpec("wall-limit", supervisor.LowCPUSleep, 3*time.Second, 256<<20, 250*time.Millisecond, "wall_limit"),
		makeSpec("multithread-cpu", supervisor.MultiThreadCPU, 2*time.Second, 256<<20, 5*time.Second, "cpu_limit"),
		makeSpec("untouched-virtual", supervisor.UntouchedMemory, 3*time.Second, 64<<20, 5*time.Second, ""),
	}
}

type resourceAPICase struct {
	ValidationError string             `json:"validation_error,omitempty"`
	ID              string             `json:"id"`
	Status          string             `json:"status"`
	Request         supervisor.Request `json:"request"`
	ExpectedReason  string             `json:"expected_reason,omitempty"`
	Result          supervisor.Result  `json:"result"`
	ErrorKinds      []string           `json:"error_kinds"`
	Error           string             `json:"error,omitempty"`
	Checks          map[string]bool    `json:"checks"`
	WallMS          int64              `json:"wall_ms"`
}
type resourceAPIReport struct {
	Schema          string            `json:"schema"`
	Status          string            `json:"status"`
	Source          string            `json:"source_commit"`
	Dirty           bool              `json:"source_dirty"`
	Environment     map[string]string `json:"environment"`
	Fingerprints    map[string]string `json:"fingerprints"`
	CounterSource   string            `json:"counter_source_commit"`
	CounterSHA      string            `json:"counter_report_sha256"`
	BusinessBackend bool              `json:"business_backend_available"`
	HardQuota       bool              `json:"hard_cpu_or_rss_quota"`
	Cases           []resourceAPICase `json:"cases"`
	Error           string            `json:"error,omitempty"`
}

func resourceErrorKinds(err error) []string {
	out := []string{}
	for _, v := range []struct {
		name string
		err  error
	}{
		{"cpu_limit", supervisor.ErrCPULimit}, {"rss_limit", supervisor.ErrRSSLimit}, {"wall_limit", supervisor.ErrWallLimit}, {"monitor_error", supervisor.ErrMonitor},
		{"cancelled", context.Canceled}, {"deadline", context.DeadlineExceeded}, {"protocol", supervisor.ErrProtocol}, {"peer_lost", supervisor.ErrPeerLost},
	} {
		if errors.Is(err, v.err) {
			out = append(out, v.name)
		}
	}
	if err != nil && len(out) == 0 {
		out = append(out, "other")
	}
	return out
}
func resourceHasKind(kinds []string, want string) bool {
	for _, kind := range kinds {
		if kind == want {
			return true
		}
	}
	return false
}

// This is the artifact oracle. Portable QA can mutate observed facts without
// confusing its synthetic fixtures with native resource enforcement evidence.
func resourceCaseChecks(c resourceAPICase, hostPID int) map[string]bool {
	r, u := c.Result, c.Result.Usage
	checks := map[string]bool{}
	checks["actual_host_and_task"] = r.HostPID == hostPID && r.Task.PID > 0 && r.Task.PID != hostPID && r.Worker.PID == r.Task.PID && r.Worker.Entered && r.Worker.Mode == c.Request.Mode
	checks["handshake"] = r.Registered && r.Armed && r.GoAuthorized && len(r.Nonce) == 32 && r.Task.Nonce == r.Nonce && r.Task.StartSeconds > 0
	wantBudget := c.Request.Budget
	if wantBudget.CPU == 0 {
		wantBudget.CPU = time.Second
	}
	if wantBudget.RSSBytes == 0 {
		wantBudget.RSSBytes = 128 << 20
	}
	if wantBudget.Wall == 0 {
		wantBudget.Wall = 3 * time.Second
	}
	checks["normalized_budget"] = r.Budget == wantBudget
	checks["real_monitor"] = u.MonitoringReady && u.MonitoringStarted && u.TimebaseNumer > 0 && u.TimebaseDenom > 0 && len(u.Samples) > 1 && len(u.Samples) <= supervisor.MaxResourceSamples
	checks["actual_wait_and_cleanup"] = r.HostWaitedGuardian && r.GuardianWaitedTask && r.TaskAbsent && r.GuardianAbsent && r.WorkRootClean && !r.Worker.ControlFDsInherited
	checks["wall_scope"] = u.GoMonotonicNS > 0 && u.WaitMonotonicNS >= u.GoMonotonicNS && u.ReceiptMonotonicNS >= u.WaitMonotonicNS && u.Wall == time.Duration(u.WaitMonotonicNS-u.GoMonotonicNS) && u.ReceiptDelay == time.Duration(u.ReceiptMonotonicNS-u.WaitMonotonicNS)
	samplesOK := true
	validAfter := 0
	var previous uint64
	var cpuExceeded, rssExceeded, retiredCPU bool
	var latestCPU, maxCPUOver time.Duration
	var peakRSS, peakVirtual, first, last uint64
	var maxGap time.Duration
	for _, s := range u.Samples {
		if s.PID != r.Task.PID || s.MonotonicNS == 0 || previous > 0 && s.MonotonicNS <= previous {
			samplesOK = false
		}
		previous = s.MonotonicNS
		if !s.Valid {
			if s.Error == "" {
				samplesOK = false
			}
			continue
		}
		raw := s.RawMachUser + s.RawMachSystem
		cpu, converted := resourceOracleNS(raw, u.TimebaseNumer, u.TimebaseDenom)
		if !converted || raw < s.RawMachUser || cpu != uint64(s.CPU) || s.RSSBytes == 0 || s.VirtualBytes < s.RSSBytes {
			samplesOK = false
		}
		if s.Phase == "running" && s.MonotonicNS >= u.GoMonotonicNS && s.MonotonicNS <= u.WaitMonotonicNS {
			latestCPU = s.CPU
			if s.RSSBytes > peakRSS {
				peakRSS = s.RSSBytes
			}
			if s.VirtualBytes > peakVirtual {
				peakVirtual = s.VirtualBytes
			}
			if first == 0 {
				first = s.MonotonicNS
			}
			if last != 0 && time.Duration(s.MonotonicNS-last) > maxGap {
				maxGap = time.Duration(s.MonotonicNS - last)
			}
			last = s.MonotonicNS
			if s.CPU > r.Budget.CPU && s.CPU-r.Budget.CPU > maxCPUOver {
				maxCPUOver = s.CPU - r.Budget.CPU
			}
			validAfter++
			if s.CPU >= r.Budget.CPU && s.MonotonicNS <= u.TriggerMonotonicNS {
				cpuExceeded = true
			}
			if s.RSSBytes > r.Budget.RSSBytes && s.MonotonicNS <= u.TriggerMonotonicNS {
				rssExceeded = true
			}
			total, live := s.RawMachUser+s.RawMachSystem, s.RawMachLiveThreadsUser+s.RawMachLiveThreadsSystem
			retiredNS, retiredValid := resourceOracleNS(total-live, u.TimebaseNumer, u.TimebaseDenom)
			if total > live && retiredValid && retiredNS >= uint64(50*time.Millisecond) {
				retiredCPU = true
			}
		}
	}
	checks["raw_sample_contract"] = samplesOK && validAfter > 0
	checks["usage_matches_active_samples"] = u.CPU == latestCPU && u.PeakRSSBytes == peakRSS && u.PeakVirtualBytes == peakVirtual && u.MaxSampleGap == maxGap && first != 0 && u.GoToFirstSample == time.Duration(first-u.GoMonotonicNS) && u.LastSampleToWait == time.Duration(u.WaitMonotonicNS-last)
	var rssOver uint64
	if peakRSS > r.Budget.RSSBytes {
		rssOver = peakRSS - r.Budget.RSSBytes
	}
	checks["overshoot_matches_samples"] = u.CPUOvershoot == maxCPUOver && u.RSSOvershootBytes == rssOver
	checks["touch_contract"] = c.Request.Mode != supervisor.TouchMemory || r.Worker.MappedBytes == 96<<20 && r.Worker.TouchedBytes > 0 && r.Worker.TouchedBytes <= r.Worker.MappedBytes
	checks["untouched_contract"] = c.Request.Mode != supervisor.UntouchedMemory || r.Worker.MappedBytes == 256<<20 && r.Worker.TouchedBytes == 0 && peakVirtual >= 256<<20 && peakRSS < r.Budget.RSSBytes
	if c.ExpectedReason == "" {
		checks["termination"] = r.State == supervisor.Completed && c.Error == "" && len(c.ErrorKinds) == 0 && r.Worker.ExitCode == 0 && r.Worker.Signal == 0 && r.StopReason == "" && u.TriggerReason == ""
	} else {
		checks["termination"] = r.State == supervisor.Failed && c.Error != "" && len(c.ErrorKinds) == 1 && resourceHasKind(c.ErrorKinds, c.ExpectedReason) && r.StopReason == c.ExpectedReason && u.TriggerReason == c.ExpectedReason && r.Worker.ExitCode == -1 && r.Worker.Signal == 9
		checks["online_trigger"] = u.TriggerMonotonicNS >= u.GoMonotonicNS && u.TriggerMonotonicNS <= u.SignalAttemptMonotonicNS && u.SignalAttemptMonotonicNS <= u.SignalReturnMonotonicNS && u.SignalAttemptMonotonicNS <= u.WaitMonotonicNS && u.SignalSent
		switch c.ExpectedReason {
		case "cpu_limit":
			checks["target_budget_observed"] = cpuExceeded
		case "rss_limit":
			checks["target_budget_observed"] = rssExceeded
		case "wall_limit":
			checks["target_budget_observed"] = time.Duration(u.TriggerMonotonicNS-u.GoMonotonicNS) >= r.Budget.Wall
		}
	}
	checks["multi_retired_thread"] = c.Request.Mode != supervisor.MultiThreadCPU || retiredCPU
	checks["case_deadline"] = c.WallMS >= 0 && c.WallMS < 8000
	return checks
}

// Independent integer conversion avoids rounding away raw Mach evidence.
func resourceOracleNS(ticks uint64, numer, denom uint32) (uint64, bool) {
	if numer == 0 || denom == 0 {
		return 0, false
	}
	hi, lo := bits.Mul64(ticks, uint64(numer))
	if hi >= uint64(denom) {
		return 0, false
	}
	v, _ := bits.Div64(hi, lo, uint64(denom))
	return v, true
}
