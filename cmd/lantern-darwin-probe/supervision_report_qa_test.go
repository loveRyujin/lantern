package main

import (
	"sort"
	"strings"
	"testing"
	"time"
)

var qaSupervisionIDs = []string{"startup-before-spawn-app", "startup-before-spawn-guardian", "startup-after-spawn-app", "startup-after-spawn-guardian", "startup-registered-app", "startup-registered-guardian", "startup-armed-app", "startup-armed-guardian", "steady-app", "steady-guardian", "setsid-app", "setsid-guardian", "setpgid-app", "setpgid-guardian", "normal", "error", "cancel", "exit-cancel-race"}
var qaSupervisionChecks = []string{"protocol_events", "go_gate", "payload_contract", "fault_response", "processes_absent", "mechanism_cleanup", "no_harness_rescue", "deadline"}
var qaSupervisionPreflight = []string{"platform", "runtime_prepared", "host_fork_positive", "policy_declared", "source_metadata"}
var qaSupervisionHashes = []string{"protocol_sha256", "payload_sha256", "policy_sha256", "runtime_marker_sha256", "executable_sha256"}

// Synthetic complete evidence exercises validation only; it makes no native
// execution claim. A fresh valid report is checked before every negative batch.
func qaSupervisionValidReport() SupervisionReport {
	r := newSupervisionReport()
	r.Status = "passed"
	r.Cleanup = true
	r.Environment = map[string]string{"os": "darwin", "arch": "arm64", "macos_version": "fixture", "macos_build": "fixture", "source_commit": strings.Repeat("ab", 20), "source_dirty": "false"}
	for _, name := range qaSupervisionPreflight {
		r.Preflight = append(r.Preflight, Check{Name: name, Passed: true})
	}
	for _, name := range qaSupervisionHashes {
		r.Fingerprints[name] = strings.Repeat("ab", 32)
	}
	r.Fingerprints["protocol_sha256"] = digest([]byte(supervisionProtocol))
	for i, id := range qaSupervisionIDs {
		r.Cases = append(r.Cases, qaSupervisionValidCase(id, 1000+i*10))
	}
	return r
}

func qaSupervisionValidCase(id string, pid int) SupervisionCase {
	c := SupervisionCase{ID: id, Status: "passed", AppPID: pid, GuardianPID: pid + 1, LauncherPID: pid + 2, AppAbsent: true, GuardianAbsent: true, PayloadAbsent: true, MechanismCleanup: true, TaskClean: true, WallMS: 100, ResponseMS: 1, Receipts: map[string]supervisionReceipt{}}
	window := strings.TrimPrefix(id, "startup-")
	if strings.HasSuffix(window, "-guardian") {
		c.FaultTarget = "guardian"
		window = strings.TrimSuffix(window, "-guardian")
	} else if strings.HasSuffix(window, "-app") {
		c.FaultTarget = "app"
		window = strings.TrimSuffix(window, "-app")
	}
	c.Window = window
	for _, name := range qaSupervisionChecks {
		c.Checks = append(c.Checks, Check{Name: name, Passed: true})
	}
	seq := map[string]int{}
	stamp := time.Now().Add(-time.Second).UnixNano()
	actorPID := map[string]int{"app": c.AppPID, "guardian": c.GuardianPID, "launcher": c.LauncherPID, "harness": 9000}
	emit := func(actor, kind string, target int) {
		seq[actor]++
		stamp += 1000
		c.Events = append(c.Events, SupervisionEvent{Actor: actor, Seq: seq[actor], AtNS: stamp, Kind: kind, PID: actorPID[actor], TargetPID: target})
	}
	emit("app", "app_started", 0)
	emit("app", "guardian_started", c.GuardianPID)
	if window == "before-spawn" {
		c.LauncherPID = 0
	} else {
		emit("guardian", "launcher_spawned", c.LauncherPID)
		emit("launcher", "prepared", c.LauncherPID)
		if window != "after-spawn" {
			c.Registered = true
			emit("app", "registered", c.LauncherPID)
			if window != "registered" {
				c.Armed = true
				emit("guardian", "armed", c.LauncherPID)
				emit("app", "armed", c.LauncherPID)
				if window != "armed" {
					c.GoAuthorized = true
					c.PayloadEntered = true
					c.PayloadPID = c.LauncherPID
					emit("app", "go_authorized", c.LauncherPID)
					emit("launcher", "go_received", c.LauncherPID)
					emit("harness", "payload_entered", c.PayloadPID)
					mode := window
					if mode == "steady" || mode == "cancel" {
						mode = "sleep"
					}
					if mode == "exit-cancel-race" {
						mode = "race"
					}
					c.Payload = &supervisionMarker{PID: c.PayloadPID, Mode: mode, StdinEmpty: true, ForkErrno: 1, SpawnErrno: 1, OriginalGroup: 800, Group: 800, Session: 700}
					if window == "setsid" || window == "setpgid" {
						c.Payload.Detached = true
						c.Payload.Group = c.PayloadPID
					}
					if window == "setsid" {
						c.Payload.Session = c.PayloadPID
					}
				}
			}
		}
	}
	identity := supervisionIdentity{PID: c.LauncherPID, StartSeconds: 1700000000, StartMicroseconds: 42, Image: "/trusted/probe", Nonce: strings.Repeat("ab", 20)}
	guardian := supervisionReceipt{Actor: "guardian", PID: c.GuardianPID, Target: identity, Waited: c.LauncherPID > 0, WaitedPID: c.LauncherPID, TargetAbsent: true, Cleanup: true, Registered: c.Registered, Armed: c.Armed, Go: c.GoAuthorized}
	app := supervisionReceipt{Actor: "app", PID: c.AppPID, Target: identity, Waited: true, WaitedPID: c.GuardianPID, TargetAbsent: true, Cleanup: true, Registered: c.Registered, Armed: c.Armed, Go: c.GoAuthorized}
	if c.FaultTarget != "" {
		windowActor := "harness"
		if window == "before-spawn" || window == "after-spawn" {
			windowActor = "guardian"
		} else if window == "registered" || window == "armed" {
			windowActor = "app"
		}
		emit(windowActor, "fault_window", c.LauncherPID)
		c.Events[len(c.Events)-1].Detail = window
		target := c.AppPID
		if c.FaultTarget == "guardian" {
			target = c.GuardianPID
		}
		emit("harness", "fault_injected", target)
		c.FaultAtNS = stamp
		if c.FaultTarget == "app" {
			guardian.StopReason = "app_eof"
			emit("guardian", "target_kill", c.LauncherPID)
			if c.LauncherPID > 0 {
				emit("guardian", "target_waited", c.LauncherPID)
			}
			c.Receipts["guardian"] = guardian
			emit("guardian", "cleanup", c.LauncherPID)
			emit("guardian", "complete", c.LauncherPID)
		} else {
			app.StopReason = "guardian_lost"
			emit("app", "peer_lost", c.GuardianPID)
			emit("app", "target_kill", c.LauncherPID)
			emit("app", "target_absent", c.LauncherPID)
			emit("app", "target_waited", c.GuardianPID)
			c.Receipts["app"] = app
			emit("app", "cleanup", c.LauncherPID)
			emit("app", "complete", c.LauncherPID)
		}
	} else {
		if id == "cancel" || id == "exit-cancel-race" {
			emit("app", "cancel_requested", c.LauncherPID)
			guardian.StopReason = "cancel"
			guardian.ExitCode = -1
		}
		if id == "error" {
			guardian.ExitCode = 1
		}
		emit("guardian", "target_waited", c.LauncherPID)
		emit("guardian", "cleanup", c.LauncherPID)
		emit("guardian", "complete", c.LauncherPID)
		emit("app", "target_waited", c.GuardianPID)
		emit("app", "cleanup", c.LauncherPID)
		emit("app", "complete", c.LauncherPID)
		c.Receipts["guardian"] = guardian
		c.Receipts["app"] = app
	}
	return c
}

func qaSupervisionCase(r *SupervisionReport, id string) *SupervisionCase {
	for i := range r.Cases {
		if r.Cases[i].ID == id {
			return &r.Cases[i]
		}
	}
	panic("bad test case ID")
}

func qaSupervisionMoveEvent(c *SupervisionCase, kind, anchor string, delta int64) {
	var at int64
	for _, e := range c.Events {
		if e.Kind == anchor {
			at = e.AtNS
			break
		}
	}
	for i := range c.Events {
		if c.Events[i].Kind == kind {
			c.Events[i].AtNS = at + delta
			if kind == "fault_injected" {
				c.FaultAtNS = at + delta
			}
			break
		}
	}
	sort.SliceStable(c.Events, func(i, j int) bool { return c.Events[i].AtNS < c.Events[j].AtNS })
}

func TestSupervisionQACompleteEvidenceValidates(t *testing.T) {
	r := qaSupervisionValidReport()
	finalizeSupervision(&r)
	if r.Status != "passed" || !r.SingleFaultVerified || r.Production || len(r.Cases) != 18 {
		t.Fatalf("complete synthetic validation fixture failed: status=%s", r.Status)
	}
	for _, c := range r.Cases {
		if err := validateSupervisionCase(c); err != nil {
			t.Fatalf("invalid fixture %s: %v", c.ID, err)
		}
	}
}

func TestSupervisionQAExitRaceAllowsNaturalAndCancelOutcomes(t *testing.T) {
	for _, stop := range []string{"", "cancel"} {
		t.Run("natural-exit-stop-"+stop, func(t *testing.T) {
			r := qaSupervisionValidReport()
			c := qaSupervisionCase(&r, "exit-cancel-race")
			v := c.Receipts["guardian"]
			v.ExitCode = 0
			v.StopReason = stop
			c.Receipts["guardian"] = v
			finalizeSupervision(&r)
			if r.Status != "passed" || !r.SingleFaultVerified {
				t.Fatalf("natural exit raced with the recorded cancel request was rejected: stop=%q", stop)
			}
		})
	}
}

func TestSupervisionQAReportFailsClosed(t *testing.T) {
	valid := qaSupervisionValidReport()
	finalizeSupervision(&valid)
	if valid.Status != "passed" {
		t.Fatal("negative batch has no valid baseline")
	}
	cases := map[string]func(*SupervisionReport){
		"missing-case":           func(r *SupervisionReport) { r.Cases = r.Cases[:17] },
		"duplicate-case":         func(r *SupervisionReport) { r.Cases[1].ID = r.Cases[0].ID },
		"unknown-case":           func(r *SupervisionReport) { r.Cases[0].ID = "unknown" },
		"unexecuted-case":        func(r *SupervisionReport) { r.Cases[0].Status = "not_run" },
		"missing-preflight":      func(r *SupervisionReport) { r.Preflight = r.Preflight[:4] },
		"unknown-preflight":      func(r *SupervisionReport) { r.Preflight[0].Name = "unknown" },
		"duplicate-preflight":    func(r *SupervisionReport) { r.Preflight[1].Name = r.Preflight[0].Name },
		"failed-preflight":       func(r *SupervisionReport) { r.Preflight[0].Passed = false },
		"missing-hash":           func(r *SupervisionReport) { delete(r.Fingerprints, "protocol_sha256") },
		"invalid-hash":           func(r *SupervisionReport) { r.Fingerprints["payload_sha256"] = strings.Repeat("z", 64) },
		"missing-check":          func(r *SupervisionReport) { r.Cases[0].Checks = r.Cases[0].Checks[:7] },
		"unknown-check":          func(r *SupervisionReport) { r.Cases[0].Checks[0].Name = "unknown" },
		"duplicate-check":        func(r *SupervisionReport) { r.Cases[0].Checks[1].Name = r.Cases[0].Checks[0].Name },
		"failed-check":           func(r *SupervisionReport) { r.Cases[0].Checks[0].Passed = false },
		"schema":                 func(r *SupervisionReport) { r.Schema = "unknown" },
		"production":             func(r *SupervisionReport) { r.Production = true },
		"global-cleanup":         func(r *SupervisionReport) { r.Cleanup = false },
		"global-error":           func(r *SupervisionReport) { r.Error = "experiment interrupted" },
		"case-error":             func(r *SupervisionReport) { r.Cases[0].Error = "failed receipt" },
		"harness-rescue":         func(r *SupervisionReport) { r.Cases[0].HarnessRescue = true },
		"task-remains":           func(r *SupervisionReport) { r.Cases[0].TaskClean = false },
		"app-remains":            func(r *SupervisionReport) { r.Cases[0].AppAbsent = false },
		"guardian-remains":       func(r *SupervisionReport) { r.Cases[0].GuardianAbsent = false },
		"payload-remains":        func(r *SupervisionReport) { r.Cases[0].PayloadAbsent = false },
		"mechanism-incomplete":   func(r *SupervisionReport) { r.Cases[0].MechanismCleanup = false },
		"go-before-registration": func(r *SupervisionReport) { qaSupervisionCase(r, "normal").Registered = false },
		"go-before-armed":        func(r *SupervisionReport) { qaSupervisionCase(r, "normal").Armed = false },
		"startup-entered":        func(r *SupervisionReport) { qaSupervisionCase(r, "startup-armed-app").PayloadEntered = true },
		"startup-go":             func(r *SupervisionReport) { qaSupervisionCase(r, "startup-armed-app").GoAuthorized = true },
		"post-go-no-marker":      func(r *SupervisionReport) { qaSupervisionCase(r, "normal").Payload = nil },
		"fd3-leak":               func(r *SupervisionReport) { qaSupervisionCase(r, "normal").Payload.OpenFDs = []int{3} },
		"stdin-not-empty":        func(r *SupervisionReport) { qaSupervisionCase(r, "normal").Payload.StdinEmpty = false },
		"fork-allowed":           func(r *SupervisionReport) { qaSupervisionCase(r, "normal").Payload.ForkErrno = 0 },
		"spawn-allowed":          func(r *SupervisionReport) { qaSupervisionCase(r, "normal").Payload.SpawnErrno = 0 },
		"wrong-payload-pid":      func(r *SupervisionReport) { qaSupervisionCase(r, "normal").Payload.PID++ },
		"payload-not-launcher": func(r *SupervisionReport) {
			c := qaSupervisionCase(r, "normal")
			c.PayloadPID++
			c.Payload.PID = c.PayloadPID
		},
		"normal-error-exit": func(r *SupervisionReport) {
			c := qaSupervisionCase(r, "normal")
			v := c.Receipts["guardian"]
			v.ExitCode = 1
			c.Receipts["guardian"] = v
		},
		"error-success-exit": func(r *SupervisionReport) {
			c := qaSupervisionCase(r, "error")
			v := c.Receipts["guardian"]
			v.ExitCode = 0
			c.Receipts["guardian"] = v
		},
		"error-signalled-exit": func(r *SupervisionReport) {
			c := qaSupervisionCase(r, "error")
			v := c.Receipts["guardian"]
			v.ExitCode = -1
			c.Receipts["guardian"] = v
		},
		"error-unrelated-exit-two": func(r *SupervisionReport) {
			c := qaSupervisionCase(r, "error")
			v := c.Receipts["guardian"]
			v.ExitCode = 2
			c.Receipts["guardian"] = v
		},
		"error-resource-stop": func(r *SupervisionReport) {
			c := qaSupervisionCase(r, "error")
			v := c.Receipts["guardian"]
			v.StopReason = "resource_limit"
			c.Receipts["guardian"] = v
		},
		"cancel-unrelated-exit": func(r *SupervisionReport) {
			c := qaSupervisionCase(r, "cancel")
			v := c.Receipts["guardian"]
			v.ExitCode = 1
			v.StopReason = ""
			c.Receipts["guardian"] = v
		},
		"cancel-error-exit-with-cancel-stop": func(r *SupervisionReport) {
			c := qaSupervisionCase(r, "cancel")
			v := c.Receipts["guardian"]
			v.ExitCode = 1
			c.Receipts["guardian"] = v
		},
		"race-error-exit-with-cancel-stop": func(r *SupervisionReport) {
			c := qaSupervisionCase(r, "exit-cancel-race")
			v := c.Receipts["guardian"]
			v.ExitCode = 7
			c.Receipts["guardian"] = v
		},
		"race-success-with-resource-stop": func(r *SupervisionReport) {
			c := qaSupervisionCase(r, "exit-cancel-race")
			v := c.Receipts["guardian"]
			v.ExitCode = 0
			v.StopReason = "resource_limit"
			c.Receipts["guardian"] = v
		},
		"race-unrelated-error": func(r *SupervisionReport) {
			c := qaSupervisionCase(r, "exit-cancel-race")
			v := c.Receipts["guardian"]
			v.ExitCode = 2
			v.StopReason = ""
			c.Receipts["guardian"] = v
		},
		"registered-before-prepared": func(r *SupervisionReport) {
			qaSupervisionMoveEvent(qaSupervisionCase(r, "normal"), "registered", "prepared", -1)
		},
		"go-before-guardian-arm-event": func(r *SupervisionReport) {
			qaSupervisionMoveEvent(qaSupervisionCase(r, "normal"), "armed", "go_authorized", 1)
		},
		"fault-before-window": func(r *SupervisionReport) {
			qaSupervisionMoveEvent(qaSupervisionCase(r, "startup-registered-app"), "fault_injected", "registered", -1)
		},
		"wrong-go-target": func(r *SupervisionReport) {
			c := qaSupervisionCase(r, "normal")
			for i := range c.Events {
				if c.Events[i].Kind == "go_authorized" {
					c.Events[i].TargetPID = c.AppPID
				}
			}
		},
		"not-detached": func(r *SupervisionReport) { qaSupervisionCase(r, "setsid-app").Payload.Detached = false },
		"detached-noop": func(r *SupervisionReport) {
			c := qaSupervisionCase(r, "setsid-app")
			c.Payload.Group = c.Payload.OriginalGroup
		},
		"case-window-mismatch": func(r *SupervisionReport) {
			c := qaSupervisionValidCase("steady-app", 1000)
			c.ID = "startup-before-spawn-app"
			r.Cases[0] = c
		},
		"missing-events":  func(r *SupervisionReport) { r.Cases[0].Events = nil },
		"unknown-actor":   func(r *SupervisionReport) { r.Cases[0].Events[0].Actor = "foreign" },
		"bad-sequence":    func(r *SupervisionReport) { r.Cases[0].Events[0].Seq = 8 },
		"wrong-actor-pid": func(r *SupervisionReport) { r.Cases[0].Events[0].PID = r.Cases[0].GuardianPID },
		"wall-deadline":   func(r *SupervisionReport) { r.Cases[0].WallMS = 8001 },
		"fault-deadline":  func(r *SupervisionReport) { r.Cases[0].ResponseMS = 2001 },
		"missing-fault":   func(r *SupervisionReport) { r.Cases[0].FaultAtNS = 0 },
		"false-orphan-wait": func(r *SupervisionReport) {
			c := qaSupervisionCase(r, "steady-guardian")
			v := c.Receipts["app"]
			v.WaitedPID = c.LauncherPID
			c.Receipts["app"] = v
		},
		"missing-direct-wait": func(r *SupervisionReport) {
			c := qaSupervisionCase(r, "steady-app")
			v := c.Receipts["guardian"]
			v.Waited = false
			c.Receipts["guardian"] = v
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			r := qaSupervisionValidReport()
			change(&r)
			finalizeSupervision(&r)
			if r.Status == "passed" || r.SingleFaultVerified {
				t.Fatalf("invalid raw evidence passed: %s", name)
			}
		})
	}
}
