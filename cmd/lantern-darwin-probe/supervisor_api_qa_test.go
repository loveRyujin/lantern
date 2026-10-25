package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loveRyujin/lantern/internal/supervisor"
)

// These are synthetic acceptance-oracle fixtures, not evidence of native Run.
func qaAPIValidCase(mode supervisor.Mode) apiCase {
	c := apiCase{ID: string(mode), HostPID: 1001, GuardianPID: 1002, TaskPID: 1003, Request: supervisor.Request{Kind: supervisor.GoLifecycle, Mode: mode}}
	c.Return = apiHostReturn{Returned: true, HostPID: c.HostPID, Result: supervisor.Result{State: supervisor.Completed, HostPID: c.HostPID, GuardianPID: c.GuardianPID, Registered: true, Armed: true, GoAuthorized: true, Worker: supervisor.WorkerResult{Entered: true, PID: c.TaskPID, Mode: mode}}}
	c.Return.Result.Nonce = strings.Repeat("a", 32)
	c.Return.Result.Task = supervisor.Identity{PID: c.TaskPID, StartSeconds: 1700000000, StartMicroseconds: 123, Image: filepath.Join(os.TempDir(), "owned-sealed-helper"), Nonce: c.Return.Result.Nonce}
	seq := map[string]int{}
	stamp := int64(1700000000000000000)
	actorPID := map[string]int{"host": c.HostPID, "guardian": c.GuardianPID, "launcher": c.TaskPID}
	emit := func(actor, kind string, target int) {
		seq[actor]++
		stamp += 1000
		c.Events = append(c.Events, supervisor.Event{Actor: actor, Seq: seq[actor], AtNS: stamp, Kind: kind, PID: actorPID[actor], TargetPID: target})
	}
	emit("host", "host_started", 0)
	emit("host", "guardian_started", c.GuardianPID)
	emit("guardian", "launcher_spawned", c.TaskPID)
	emit("host", "launcher_observed", c.TaskPID)
	emit("launcher", "prepared", c.TaskPID)
	emit("host", "registered", c.TaskPID)
	emit("guardian", "armed", c.TaskPID)
	emit("host", "armed", c.TaskPID)
	emit("host", "go_authorized", c.TaskPID)
	emit("launcher", "go_received", c.TaskPID)
	emit("guardian", "payload_entered", c.TaskPID)
	emit("host", "payload_entered", c.TaskPID)
	if mode == supervisor.ExpectedError {
		c.Return.Result.Worker.ExitCode = 1
	}
	if mode == supervisor.Hold || mode == supervisor.ExitRace {
		c.CancelAttempted = true
		emit("host", "cancel_requested", c.TaskPID)
		c.Return.Result.State = supervisor.Cancelled
		c.Return.Result.Worker.ExitCode = -1
		c.Return.Result.Worker.Signal = 9
		c.Return.Result.StopReason = "cancel"
		c.Return.Error = context.Canceled.Error()
	}
	return c
}

func qaAPIEvent(c *apiCase, actor, kind string) *supervisor.Event {
	for i := range c.Events {
		if c.Events[i].Actor == actor && c.Events[i].Kind == kind {
			return &c.Events[i]
		}
	}
	panic("unknown QA fixture event")
}

func TestSupervisorAPIQAValidOracleFixtures(t *testing.T) {
	for _, mode := range []supervisor.Mode{supervisor.Normal, supervisor.ExpectedError, supervisor.Hold, supervisor.ExitRace} {
		c := qaAPIValidCase(mode)
		if !apiProtocolOrder(c) || !apiOutcome(c, mode == supervisor.Hold || mode == supervisor.ExitRace) {
			t.Fatalf("complete synthetic fixture failed for %s", mode)
		}
	}
	for _, stop := range []string{"", "cancel"} {
		c := qaAPIValidCase(supervisor.ExitRace)
		c.Return.Result.State = supervisor.Completed
		c.Return.Result.Worker.ExitCode = 0
		c.Return.Result.Worker.Signal = 0
		c.Return.Result.StopReason = stop
		c.Return.Error = ""
		if !apiOutcome(c, true) {
			t.Fatalf("natural exit raced with actual cancellation attempt was rejected: stop=%q", stop)
		}
	}
	// The caller's cancellation can win its own terminal state while the
	// already-released fixed race worker exits naturally. Neither fact changes
	// the other: the worker stays exit 0 and the caller stays context-cancelled.
	c := qaAPIValidCase(supervisor.ExitRace)
	c.Return.Result.Worker.ExitCode = 0
	c.Return.Result.Worker.Signal = 0
	if !apiOutcome(c, true) {
		t.Fatal("independent supervisor cancellation and natural race worker exit were conflated")
	}
}

func TestSupervisorAPIQARawOrderRejectsContradictions(t *testing.T) {
	changes := map[string]func(*apiCase){
		"no-chain-only-host-starts": func(c *apiCase) { c.Events = c.Events[:2] },
		"foreign-caller-pid":        func(c *apiCase) { c.Events[0].PID = c.GuardianPID },
		"bad-actor-sequence":        func(c *apiCase) { c.Events[0].Seq = 2 },
		"wrong-go-target":           func(c *apiCase) { qaAPIEvent(c, "host", "go_authorized").TargetPID = c.GuardianPID },
		"wrong-bootstrap-target":    func(c *apiCase) { qaAPIEvent(c, "host", "launcher_observed").TargetPID = c.GuardianPID },
		"bootstrap-after-registration": func(c *apiCase) {
			qaAPIEvent(c, "host", "launcher_observed").AtNS = qaAPIEvent(c, "host", "registered").AtNS + 1
		},
		"registration-before-prepared": func(c *apiCase) {
			qaAPIEvent(c, "launcher", "prepared").AtNS = qaAPIEvent(c, "host", "registered").AtNS + 1
		},
		"guardian-arm-before-registration": func(c *apiCase) {
			qaAPIEvent(c, "guardian", "armed").AtNS = qaAPIEvent(c, "host", "registered").AtNS - 1
		},
		"go-before-host-ack": func(c *apiCase) {
			qaAPIEvent(c, "host", "go_authorized").AtNS = qaAPIEvent(c, "host", "armed").AtNS - 1
		},
		"payload-before-go": func(c *apiCase) {
			qaAPIEvent(c, "host", "payload_entered").AtNS = qaAPIEvent(c, "launcher", "go_received").AtNS - 1
		},
		"startup-armed-actually-goes": func(c *apiCase) {
			c.ID = "startup-armed-guardian"
			c.Request.Gate = supervisor.Armed
			c.FaultTarget = "guardian"
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			c := qaAPIValidCase(supervisor.Normal)
			if !apiProtocolOrder(c) {
				t.Fatal("negative raw-order fixture has no valid control")
			}
			change(&c)
			if apiProtocolOrder(c) {
				t.Fatal("contradictory native evidence was accepted")
			}
		})
	}
}

func TestSupervisorAPIQAWorkerOutcomesRemainDistinct(t *testing.T) {
	for _, test := range []struct {
		name   string
		mode   supervisor.Mode
		change func(*apiCase)
	}{
		{"error-signal-is-not-fixed-runtime-error", supervisor.ExpectedError, func(c *apiCase) { c.Return.Result.Worker.ExitCode = -1; c.Return.Result.Worker.Signal = 9 }},
		{"error-exit-two", supervisor.ExpectedError, func(c *apiCase) { c.Return.Result.Worker.ExitCode = 2 }},
		{"error-resource-stop", supervisor.ExpectedError, func(c *apiCase) { c.Return.Result.StopReason = "wall_limit" }},
		{"cancel-worker-exit-one", supervisor.Hold, func(c *apiCase) { c.Return.Result.Worker.ExitCode = 1 }},
		{"ordinary-cancel-worker-exit-zero", supervisor.Hold, func(c *apiCase) { c.Return.Result.Worker.ExitCode = 0; c.Return.Result.Worker.Signal = 0 }},
		{"cancel-without-signal", supervisor.Hold, func(c *apiCase) { c.Return.Result.Worker.Signal = 0 }},
		{"cancel-reports-protocol-error", supervisor.Hold, func(c *apiCase) { c.Return.Error = supervisor.ErrProtocol.Error() }},
		{"cancel-masks-joined-protocol-error", supervisor.Hold, func(c *apiCase) { c.Return.Error = context.Canceled.Error() + "\n" + supervisor.ErrProtocol.Error() }},
		{"cancel-wrong-task", supervisor.Hold, func(c *apiCase) { qaAPIEvent(c, "host", "cancel_requested").TargetPID = c.GuardianPID }},
		{"cancel-not-attempted", supervisor.Hold, func(c *apiCase) { c.CancelAttempted = false }},
		{"race-exit-seven-with-cancel", supervisor.ExitRace, func(c *apiCase) { c.Return.Result.Worker.ExitCode = 7 }},
		{"race-natural-cancel-protocol-error", supervisor.ExitRace, func(c *apiCase) {
			c.Return.Result.Worker.ExitCode = 0
			c.Return.Result.Worker.Signal = 0
			c.Return.Error = supervisor.ErrProtocol.Error()
		}},
		{"race-natural-resource-stop", supervisor.ExitRace, func(c *apiCase) {
			c.Return.Result.State = supervisor.Completed
			c.Return.Result.Worker.ExitCode = 0
			c.Return.Result.Worker.Signal = 0
			c.Return.Result.StopReason = "resource_limit"
			c.Return.Error = ""
		}},
		{"worker-mode-mismatch", supervisor.Normal, func(c *apiCase) { c.Return.Result.Worker.Mode = supervisor.Hold }},
		{"worker-pid-mismatch", supervisor.Normal, func(c *apiCase) { c.Return.Result.Worker.PID = c.GuardianPID }},
		{"worker-control-fd-inherited", supervisor.Normal, func(c *apiCase) { c.Return.Result.Worker.ControlFDsInherited = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qaAPIValidCase(test.mode)
			cancellation := test.mode == supervisor.Hold || test.mode == supervisor.ExitRace
			if !apiOutcome(c, cancellation) {
				t.Fatal("negative outcome fixture has no valid control")
			}
			test.change(&c)
			if apiOutcome(c, cancellation) {
				t.Fatal("worker/host failure was accepted as the requested fixed outcome")
			}
		})
	}
}

func qaAPIStartup(gate supervisor.Gate, target string) apiCase {
	c := qaAPIValidCase(supervisor.Normal)
	c.ID = "startup-" + string(gate) + "-" + target
	c.Request = supervisor.Request{Kind: supervisor.GoLifecycle, Mode: supervisor.Hold, Gate: gate}
	c.FaultTarget = target
	end, actor := 2, "guardian"
	switch gate {
	case supervisor.BeforeSpawn:
		c.TaskPID = 0
		c.Return.Result.Task = supervisor.Identity{}
	case supervisor.AfterSpawn:
		end = 4
	case supervisor.Registered:
		end, actor = 6, "host"
	case supervisor.Armed:
		end, actor = 8, "host"
	}
	c.Events = c.Events[:end]
	seq := 1
	for _, e := range c.Events {
		if e.Actor == actor {
			seq = e.Seq + 1
		}
	}
	pid := c.GuardianPID
	if actor == "host" {
		pid = c.HostPID
	}
	at := c.Events[len(c.Events)-1].AtNS + 1000
	c.Events = append(c.Events, supervisor.Event{Actor: actor, Seq: seq, AtNS: at, Kind: "fault_window", PID: pid, TargetPID: c.TaskPID, Detail: string(gate)})
	c.FaultAtNS = at + 1000
	c.Return.Result.Worker.Entered = false
	c.Return.Result.GoAuthorized = false
	c.Return.Result.State = supervisor.Failed
	c.Return.Result.StopReason = "guardian_lost"
	c.Return.Result.HostWaitedGuardian = true
	c.Return.Result.GuardianWaitedTask = false
	c.Return.Result.TaskAbsent = true
	c.Return.Result.GuardianAbsent = true
	c.Return.Error = supervisor.ErrPeerLost.Error()
	if target == "host" {
		id := c.Return.Result.Task
		if id.PID == 0 {
			id.Nonce = c.Return.Result.Nonce
		}
		c.GuardianReceipt, _ = json.Marshal(map[string]any{"target": id, "waited": c.TaskPID > 0, "absent": true, "clean": true})
		c.Return = apiHostReturn{}
	}
	return c
}

func TestSupervisorAPIQAStartupWindowsHaveValidControls(t *testing.T) {
	for _, gate := range []supervisor.Gate{supervisor.BeforeSpawn, supervisor.AfterSpawn, supervisor.Registered, supervisor.Armed} {
		for _, target := range []string{"host", "guardian"} {
			c := qaAPIStartup(gate, target)
			if !apiProtocolOrder(c) || !apiFaultContract(c) {
				t.Fatalf("valid synthetic startup window rejected: %s", c.ID)
			}
		}
	}
}

func TestSupervisorAPIQAFaultEvidenceRejectsFalsePhaseAndWait(t *testing.T) {
	for name, change := range map[string]func(*apiCase){
		"wrong-window-actor": func(c *apiCase) {
			e := qaAPIEvent(c, "host", "fault_window")
			e.Actor, e.PID = "guardian", c.GuardianPID
		},
		"wrong-window-target": func(c *apiCase) { qaAPIEvent(c, "host", "fault_window").TargetPID = c.GuardianPID },
		"wrong-window-detail": func(c *apiCase) { qaAPIEvent(c, "host", "fault_window").Detail = "armed" },
		"fault-before-window": func(c *apiCase) { c.FaultAtNS = qaAPIEvent(c, "host", "fault_window").AtNS - 1 },
		"registration-not-yet-reached": func(c *apiCase) {
			qaAPIEvent(c, "host", "registered").AtNS = qaAPIEvent(c, "host", "fault_window").AtNS + 1
		},
		"advanced-arm-after-window": func(c *apiCase) {
			c.Events = append(c.Events, supervisor.Event{Actor: "guardian", Seq: 2, AtNS: c.FaultAtNS + 1000, Kind: "armed", PID: c.GuardianPID, TargetPID: c.TaskPID})
		},
		"fake-guardian-waited-orphan":  func(c *apiCase) { c.Return.Result.GuardianWaitedTask = true },
		"guardian-loss-no-host-wait":   func(c *apiCase) { c.Return.Result.HostWaitedGuardian = false },
		"guardian-loss-task-remains":   func(c *apiCase) { c.Return.Result.TaskAbsent = false },
		"unrelated-resource-stop":      func(c *apiCase) { c.Return.Result.StopReason = "resource_limit" },
		"guardian-completed-success":   func(c *apiCase) { c.Return.Result.State = supervisor.Completed },
		"result-task-identity-missing": func(c *apiCase) { c.Return.Result.Task = supervisor.Identity{} },
		"result-task-wrong-pid":        func(c *apiCase) { c.Return.Result.Task.PID = c.GuardianPID },
		"result-task-missing-birth":    func(c *apiCase) { c.Return.Result.Task.StartSeconds = 0 },
		"result-task-wrong-nonce":      func(c *apiCase) { c.Return.Result.Task.Nonce = "another-job" },
		"result-task-missing-image":    func(c *apiCase) { c.Return.Result.Task.Image = "" },
	} {
		t.Run(name, func(t *testing.T) {
			c := qaAPIStartup(supervisor.Registered, "guardian")
			if !apiFaultContract(c) {
				t.Fatal("fault negative has no valid control")
			}
			change(&c)
			if apiFaultContract(c) {
				t.Fatal("false startup or phantom Wait evidence passed")
			}
		})
	}
	t.Run("host-dead-receipt-other-task", func(t *testing.T) {
		c := qaAPIStartup(supervisor.Registered, "host")
		if !apiFaultContract(c) {
			t.Fatal("host-death negative has no valid guardian receipt control")
		}
		var receipt map[string]any
		if err := json.Unmarshal(c.GuardianReceipt, &receipt); err != nil {
			t.Fatal(err)
		}
		receipt["target"].(map[string]any)["pid"] = c.GuardianPID
		c.GuardianReceipt, _ = json.Marshal(receipt)
		if apiFaultContract(c) {
			t.Fatal("host-death cleanup was attributed to a different task identity")
		}
	})
}
