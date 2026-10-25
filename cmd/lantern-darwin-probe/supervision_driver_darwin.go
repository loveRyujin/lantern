//go:build darwin

package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/loveRyujin/lantern/core/pyrt"
)

const supervisionHostFork = `import os
pid=os.fork()
if pid==0: os._exit(0)
assert os.waitpid(pid,0)[1]==0
print('owned-fork-ok')
`

func runSupervision(ctx context.Context, o options) (r SupervisionReport) {
	r = newSupervisionReport()
	r.Environment["source_commit"] = commandText(ctx, "git", "rev-parse", "HEAD")
	r.Environment["source_dirty"] = strconv.FormatBool(commandText(ctx, "git", "status", "--porcelain") != "")
	r.Environment["macos_version"] = commandText(ctx, "/usr/bin/sw_vers", "-productVersion")
	r.Environment["macos_build"] = commandText(ctx, "/usr/bin/sw_vers", "-buildVersion")
	r.Environment["uid"] = strconv.Itoa(os.Getuid())
	r.Fingerprints["protocol_sha256"] = digest([]byte(supervisionProtocol))
	r.Fingerprints["payload_sha256"] = digest(supervisionPayload)
	rt, e := readRuntime(o.RuntimeConfig)
	if e != nil {
		r.Error = e.Error()
		return
	}
	r.Runtime = rt
	r.Environment["python"] = rt.Base.Version.String()
	release, e := pyrt.AcquirePrepared(ctx, rt)
	if e != nil {
		r.Error = e.Error()
		return
	}
	defer release()
	marker, e := os.ReadFile(filepath.Join(rt.Venv, ".lantern-requirements.sha256"))
	if e != nil {
		r.Error = e.Error()
		return
	}
	r.Fingerprints["runtime_marker_sha256"] = digest(marker)
	m, e := newProcessMonitor()
	if e != nil {
		r.Error = e.Error()
		return
	}
	plan, e := runtimeReadPlan(rt)
	if e != nil {
		r.Error = e.Error()
		return
	}
	exe, e := os.Executable()
	if e != nil {
		r.Error = e.Error()
		return
	}
	exe = canonical(exe)
	b, e := os.ReadFile(exe)
	if e != nil {
		r.Error = e.Error()
		return
	}
	r.Fingerprints["executable_sha256"] = digest(b)
	out, e := filepath.Abs(o.Out)
	if e != nil {
		r.Error = e.Error()
		return
	}
	for _, d := range []string{out, filepath.Join(out, "events"), filepath.Join(out, "policies")} {
		if e = os.MkdirAll(d, 0700); e != nil {
			r.Error = e.Error()
			return
		}
	}
	base, e := os.MkdirTemp("", "lantern-supervision-")
	if e != nil {
		r.Error = e.Error()
		return
	}
	base = canonical(base)
	r.Environment["control_fixture_root"] = base
	defer func() {
		e := cleanupFixtureRoot(base)
		r.Cleanup = e == nil
		if e != nil {
			r.Status = "failed"
			r.Error = "fixture cleanup: " + e.Error()
		}
	}()
	x := &experiment{ctx: ctx, out: out, base: base, exe: exe, rt: rt, plan: plan, monitor: m}
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	host := exec.CommandContext(probe, rt.Python, "-I", "-B", "-c", supervisionHostFork)
	host.Env = fixedEnv(taskConfig{Tmp: base})
	data, he := host.Output()
	cancel()
	r.Preflight = []Check{{"platform", os.Getuid() != 0, "actual nonroot macOS; one-process faults only"}, {"runtime_prepared", true, "offline shared lease held"}, {"host_fork_positive", he == nil && strings.TrimSpace(string(data)) == "owned-fork-ok", "one owned child forked and Waited before no-fork policy"}, {"policy_declared", true, "same deny-default filesystem/network and deny process-fork profile as the separately validated baseline"}, {"source_metadata", !strings.HasPrefix(r.Environment["source_commit"], "error:"), "actual executed source commit/dirty recorded"}}
	for _, check := range r.Preflight {
		if !check.Passed {
			r.Status = "failed"
			r.Error = "supervision preflight failed: " + check.Name
			return
		}
	}
	r.Status = "failed"
	policies := []byte{}
	for _, id := range supervisionCaseIDs {
		window, fault, mode := supervisionSpec(id)
		task, e := x.task("sleep", true, exe)
		if e != nil {
			r.Error = e.Error()
			return
		}
		task.Budget.WallMS = 5000
		control := filepath.Join(out, "events", id)
		if e = os.MkdirAll(control, 0700); e != nil {
			r.Error = e.Error()
			return
		}
		var nonce [16]byte
		if _, e = rand.Read(nonce[:]); e != nil {
			r.Error = e.Error()
			return
		}
		c := supervisionConfig{id, hex.EncodeToString(nonce[:]), window, fault, mode, control, exe, task}
		if e = os.WriteFile(task.Script, supervisionPayload, 0600); e != nil {
			r.Error = e.Error()
			return
		}
		policy := []byte(makePolicy(plan, task, exe))
		if e = os.WriteFile(task.Policy, policy, 0600); e != nil {
			r.Error = e.Error()
			return
		}
		policies = append(policies, policy...)
		config := filepath.Join(control, "config.json")
		if e = writeJSON(config, c); e != nil {
			r.Error = e.Error()
			return
		}
		if e = writeJSON(filepath.Join(task.Root, "input", "supervision-task.json"), c); e != nil {
			r.Error = e.Error()
			return
		}
		r.Cases = append(r.Cases, runSupervisionCase(ctx, c, config, m))
	}
	r.Fingerprints["policy_sha256"] = digest(policies)
	return
}

func supervisionLoadEvents(dir string) []SupervisionEvent {
	var events []SupervisionEvent
	for _, actor := range []string{"app", "guardian", "launcher", "harness"} {
		f, e := os.Open(filepath.Join(dir, actor+".jsonl"))
		if e != nil {
			continue
		}
		s := bufio.NewScanner(f)
		s.Buffer(make([]byte, 1024), 16<<10)
		for s.Scan() {
			var v SupervisionEvent
			if json.Unmarshal(s.Bytes(), &v) == nil {
				events = append(events, v)
			}
		}
		f.Close()
	}
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].AtNS == events[j].AtNS {
			return events[i].Actor < events[j].Actor
		}
		return events[i].AtNS < events[j].AtNS
	})
	return events
}
func supervisionHas(events []SupervisionEvent, actor, kind string) bool {
	for _, e := range events {
		if (actor == "" || e.Actor == actor) && e.Kind == kind {
			return true
		}
	}
	return false
}
func supervisionTaskAbsent(root string) bool { _, e := os.Lstat(root); return os.IsNotExist(e) }

func runSupervisionCase(ctx context.Context, c supervisionConfig, path string, m *processMonitor) (result SupervisionCase) {
	result = SupervisionCase{ID: c.ID, Status: "failed", Window: c.Window, FaultTarget: c.Fault, Events: []SupervisionEvent{}, Checks: []Check{}, Receipts: map[string]supervisionReceipt{}}
	log := supervisionLog{c.Control, "harness", 0}
	start := time.Now()
	cmd := exec.Command(c.Exe, "__supervision-app", path)
	cmd.Env = fixedEnv(c.Task)
	diagnostic := &boundedBuffer{limit: 64 << 10, overflow: make(chan struct{})}
	cmd.Stderr = diagnostic
	if e := cmd.Start(); e != nil {
		result.Error = e.Error()
		return
	}
	result.AppPID = cmd.Process.Pid
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	appWaited := false
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	var faultTime time.Time
	refresh := func() {
		result.Events = supervisionLoadEvents(c.Control)
		for _, e := range result.Events {
			if e.Kind == "guardian_started" && e.Actor == "app" {
				result.GuardianPID = e.TargetPID
			}
			if e.Kind == "launcher_spawned" && e.Actor == "guardian" {
				result.LauncherPID = e.TargetPID
			}
		}
		result.Registered = supervisionHas(result.Events, "app", "registered")
		result.Armed = supervisionHas(result.Events, "app", "armed") && supervisionHas(result.Events, "guardian", "armed")
		result.GoAuthorized = supervisionHas(result.Events, "app", "go_authorized")
		var marker supervisionMarker
		if readJSON(filepath.Join(c.Task.Out, "marker.json"), &marker) == nil {
			_ = writeJSON(filepath.Join(c.Control, "harness-marker.json"), marker)
		} else if readJSON(filepath.Join(c.Control, "payload-marker.json"), &marker) != nil {
			_ = readJSON(filepath.Join(c.Control, "harness-marker.json"), &marker)
		}
		if marker.PID > 0 {
			if result.Payload == nil {
				log.event("payload_entered", marker.PID, "observed raw fixed payload marker")
			}
			result.Payload = &marker
			result.PayloadEntered = true
			result.PayloadPID = marker.PID
		}
		for _, actor := range []string{"app", "guardian"} {
			var receipt supervisionReceipt
			if readJSON(filepath.Join(c.Control, actor+"-receipt.json"), &receipt) == nil {
				result.Receipts[actor] = receipt
			}
		}
		result.AppAbsent = appWaited && processAbsent(m, result.AppPID)
		result.GuardianAbsent = result.GuardianPID > 0 && processAbsent(m, result.GuardianPID)
		result.PayloadAbsent = result.LauncherPID <= 0 || processAbsent(m, result.LauncherPID)
		result.TaskClean = supervisionTaskAbsent(c.Task.Root)
		owner := "app"
		if c.Fault == "app" {
			owner = "guardian"
		}
		if receipt, ok := result.Receipts[owner]; ok {
			result.MechanismCleanup = receipt.Cleanup && receipt.TargetAbsent && receipt.Error == ""
		}
	}
	finished := false
	for !finished {
		select {
		case <-done:
			appWaited = true
			done = nil
		case <-ctx.Done():
			result.Error = "outer context ended"
			finished = true
		case <-deadline.C:
			result.Error = "case exceeded 8s"
			finished = true
		case <-ticker.C:
			refresh()
			if c.Fault != "" && faultTime.IsZero() {
				ready := false
				if c.Window == "steady" || c.Window == "setsid" || c.Window == "setpgid" {
					ready = result.PayloadEntered
				} else {
					for _, e := range result.Events {
						if e.Kind == "fault_window" && e.Detail == c.Window {
							ready = true
						}
					}
				}
				if ready && result.GuardianPID > 0 {
					faultTime = time.Now()
					result.FaultAtNS = faultTime.UnixNano()
					pid := result.GuardianPID
					if c.Fault == "app" {
						pid = result.AppPID
					}
					log.event("fault_injected", pid, c.Window)
					var e error
					if c.Fault == "app" {
						e = cmd.Process.Kill()
					} else {
						var id supervisionIdentity
						id, e = supervisionIdentityNow(m, pid, c.Nonce)
						if e == nil && id.Image != canonical(c.Exe) {
							e = fmt.Errorf("guardian image mismatch")
						}
						if e == nil {
							e = supervisionKill(m, id)
						}
					}
					if e != nil {
						result.Error = "fault injection: " + e.Error()
						finished = true
					}
				}
			}
			if result.AppAbsent && result.GuardianAbsent && result.PayloadAbsent && result.TaskClean && result.MechanismCleanup {
				finished = true
			}
			if !faultTime.IsZero() && time.Since(faultTime) > 2*time.Second {
				result.Error = "fault response exceeded 2s"
				finished = true
			}
		}
	}
	refresh()
	result.WallMS = time.Since(start).Seconds() * 1000
	if !faultTime.IsZero() {
		result.ResponseMS = time.Since(faultTime).Seconds() * 1000
	}
	// Rescue is a failure-only harness action. It never upgrades a case.
	if !result.AppAbsent || !result.GuardianAbsent || !result.PayloadAbsent || !result.TaskClean {
		result.HarnessRescue = true
		_ = cmd.Process.Kill()
		if !appWaited {
			select {
			case <-done:
				appWaited = true
			case <-time.After(time.Second):
			}
		}
		for _, pid := range []int{result.GuardianPID, result.LauncherPID} {
			if pid > 0 && !processAbsent(m, pid) {
				id, e := supervisionIdentityNow(m, pid, c.Nonce)
				if e == nil {
					_ = supervisionKill(m, id)
				}
			}
		}
		for _, pid := range []int{result.GuardianPID, result.LauncherPID} {
			if pid > 0 {
				_ = supervisionGone(m, pid)
			}
		}
		_ = cleanupFixtureRoot(c.Task.Root)
	}
	goOK := (!result.GoAuthorized || result.Registered && result.Armed)
	startup := c.Window == "before-spawn" || c.Window == "after-spawn" || c.Window == "registered" || c.Window == "armed"
	if startup {
		goOK = goOK && !result.GoAuthorized && !result.PayloadEntered
	} else {
		goOK = goOK && result.GoAuthorized && result.PayloadEntered
	}
	payloadOK := startup && !result.PayloadEntered
	if p := result.Payload; p != nil {
		payloadOK = p.PID == result.LauncherPID && len(p.OpenFDs) == 0 && p.StdinEmpty && p.ForkErrno == int(syscall.EPERM) && p.SpawnErrno == int(syscall.EPERM)
		if c.Window == "setsid" || c.Window == "setpgid" {
			payloadOK = payloadOK && p.Detached
		}
	}
	responseOK := result.Error == ""
	if c.Fault != "" {
		responseOK = responseOK && !faultTime.IsZero() && result.ResponseMS <= 2000
	} else {
		g, ok := result.Receipts["guardian"]
		responseOK = responseOK && ok && g.Waited && g.WaitedPID == result.LauncherPID
		switch c.ID {
		case "normal":
			responseOK = responseOK && g.ExitCode == 0 && g.StopReason == ""
		case "error":
			responseOK = responseOK && g.ExitCode != 0 && g.StopReason == ""
		case "cancel":
			responseOK = responseOK && g.StopReason == "cancel" && g.ExitCode != 0 && supervisionHas(result.Events, "app", "cancel_requested")
		case "exit-cancel-race":
			responseOK = responseOK && supervisionHas(result.Events, "app", "cancel_requested") && (g.StopReason == "cancel" || g.ExitCode == 0)
		}
	}
	protocolErr := validateSupervisionCase(result)
	for _, v := range []Check{{"protocol_events", protocolErr == nil, fmt.Sprint(protocolErr)}, {"go_gate", goOK, "registered and armed before GO; startup faults never enter"}, {"payload_contract", payloadOK, "real marker, PID continuity, stdio-only, no-fork and actual detach"}, {"fault_response", responseOK, "declared fault/normal/error/cancel path actually observed"}, {"processes_absent", result.AppAbsent && result.GuardianAbsent && result.PayloadAbsent, "own child Wait vs orphan absence remain distinct"}, {"mechanism_cleanup", result.MechanismCleanup && result.TaskClean, "surviving mechanism performs cleanup"}, {"no_harness_rescue", !result.HarnessRescue, "failure-only rescue never counts as success"}, {"deadline", result.WallMS <= 8000 && result.ResponseMS <= 2000, "8s case; 2s fault-to-absence"}} {
		result.Checks = append(result.Checks, v)
	}
	result.Status = "passed"
	for _, v := range result.Checks {
		if !v.Passed {
			result.Status = "failed"
		}
	}
	if result.Error != "" {
		result.Status = "failed"
	}
	return
}
