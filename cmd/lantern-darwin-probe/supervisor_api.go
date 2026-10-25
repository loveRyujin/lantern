package main

import (
	"encoding/json"

	"github.com/loveRyujin/lantern/internal/supervisor"
)

type apiHostReturn struct {
	Returned bool              `json:"returned"`
	HostPID  int               `json:"host_pid"`
	Result   supervisor.Result `json:"result"`
	Error    string            `json:"error,omitempty"`
}
type apiCase struct {
	FaultAtNS       int64              `json:"fault_at_ns,omitempty"`
	AbsentAtNS      int64              `json:"absent_at_ns,omitempty"`
	FaultToAbsentMS float64            `json:"fault_to_absent_ms"`
	CancelAttempted bool               `json:"cancel_attempted"`
	ID              string             `json:"id"`
	Status          string             `json:"status"`
	Request         supervisor.Request `json:"request"`
	FaultTarget     string             `json:"fault_target,omitempty"`
	HostPID         int                `json:"host_pid"`
	GuardianPID     int                `json:"guardian_pid"`
	TaskPID         int                `json:"task_pid"`
	Return          apiHostReturn      `json:"return"`
	Events          []supervisor.Event `json:"events"`
	GuardianReceipt json.RawMessage    `json:"guardian_receipt,omitempty"`
	Checks          map[string]bool    `json:"checks"`
	HarnessRescue   bool               `json:"harness_rescue"`
	Error           string             `json:"error,omitempty"`
	WallMS          int64              `json:"wall_ms"`
}
type apiReport struct {
	Environment   map[string]string `json:"environment"`
	Fingerprints  map[string]string `json:"fingerprints"`
	Schema        string            `json:"schema"`
	Status        string            `json:"status"`
	Source        string            `json:"source_commit"`
	Dirty         bool              `json:"source_dirty"`
	ExecutableSHA string            `json:"executable_sha256"`
	Cases         []apiCase         `json:"cases"`
	Error         string            `json:"error,omitempty"`
}

// The acceptance oracle checks role/PID/order, not just the presence of words.
func apiProtocolOrder(c apiCase) bool {
	keys := map[string]int64{}
	seq := map[string]int{}
	for _, e := range c.Events {
		want := 0
		switch e.Actor {
		case "host":
			want = c.HostPID
		case "guardian":
			want = c.GuardianPID
		case "launcher":
			want = c.TaskPID
		default:
			return false
		}
		if want <= 0 || e.PID != want || e.Seq != seq[e.Actor]+1 || e.AtNS <= 0 {
			return false
		}
		seq[e.Actor] = e.Seq
		key := e.Actor + ":" + e.Kind
		if _, exists := keys[key]; exists {
			return false
		}
		keys[key] = e.AtNS
		switch e.Kind {
		case "launcher_observed", "registered", "armed", "go_authorized", "go_received", "prepared", "payload_entered":
			if e.TargetPID != c.TaskPID || c.TaskPID <= 0 {
				return false
			}
		}
	}
	before := func(a, b string) bool { return keys[a] > 0 && keys[b] > keys[a] }
	if keys["host:registered"] > 0 && (!before("launcher:prepared", "host:registered") || !before("host:launcher_observed", "host:registered")) {
		return false
	}
	if keys["host:armed"] > 0 && (!before("host:registered", "guardian:armed") || !before("guardian:armed", "host:armed")) {
		return false
	}
	if keys["host:go_authorized"] > 0 && !before("host:armed", "host:go_authorized") {
		return false
	}
	if keys["launcher:go_received"] > 0 && !before("host:go_authorized", "launcher:go_received") {
		return false
	}
	if keys["host:payload_entered"] > 0 && !before("launcher:go_received", "host:payload_entered") {
		return false
	}
	if keys["guardian:payload_entered"] > 0 && !before("launcher:go_received", "guardian:payload_entered") {
		return false
	}
	if c.Request.Gate != supervisor.NoGate && (keys["host:go_authorized"] > 0 || keys["launcher:go_received"] > 0 || keys["host:payload_entered"] > 0 || keys["guardian:payload_entered"] > 0) {
		return false
	}
	if c.Request.Gate == supervisor.NoGate {
		if keys["host:payload_entered"] == 0 || keys["launcher:go_received"] == 0 || keys["host:go_authorized"] == 0 || keys["host:armed"] == 0 || keys["guardian:armed"] == 0 || keys["host:registered"] == 0 || keys["launcher:prepared"] == 0 || keys["host:launcher_observed"] == 0 {
			return false
		}
	}
	return keys["host:host_started"] > 0 && keys["host:guardian_started"] > 0
}
func apiOutcome(c apiCase, cancellation bool) bool {
	r := c.Return.Result
	if !r.Worker.Entered || r.Worker.PID != c.TaskPID || r.Worker.Mode != c.Request.Mode || r.Worker.ControlFDsInherited {
		return false
	}
	if !cancellation {
		want := 0
		if c.Request.Mode == supervisor.ExpectedError {
			want = 1
		}
		return r.State == supervisor.Completed && c.Return.Error == "" && r.Worker.ExitCode == want && r.Worker.Signal == 0 && r.StopReason == ""
	}
	attempted := false
	for _, e := range c.Events {
		if e.Actor == "host" && e.Kind == "cancel_requested" && e.PID == c.HostPID && e.TargetPID == c.TaskPID {
			attempted = true
		}
	}
	// A completion race can win before cancellation reaches core; the fixture
	// records the actual attempt separately so a natural exit is never invented.
	if !c.CancelAttempted {
		return false
	}
	if r.State == supervisor.Cancelled {
		if !attempted || c.Return.Error != "context canceled" || r.StopReason != "cancel" {
			return false
		}
		return r.Worker.ExitCode == -1 && r.Worker.Signal == 9 || c.Request.Mode == supervisor.ExitRace && r.Worker.ExitCode == 0 && r.Worker.Signal == 0
	}
	return c.Request.Mode == supervisor.ExitRace && r.State == supervisor.Completed && c.Return.Error == "" && r.Worker.ExitCode == 0 && r.Worker.Signal == 0 && (r.StopReason == "" || r.StopReason == "cancel")
}

func apiFaultContract(c apiCase) bool {
	if c.FaultTarget == "guardian" {
		r := c.Return.Result
		if !c.Return.Returned || r.State != supervisor.Failed || c.Return.Error == "" || r.StopReason != "guardian_lost" || r.GuardianWaitedTask || !r.HostWaitedGuardian || !r.TaskAbsent || !r.GuardianAbsent {
			return false
		}
	}
	if c.Request.Gate == supervisor.NoGate {
		return true
	}
	events := map[string]supervisor.Event{}
	var window supervisor.Event
	for _, e := range c.Events {
		events[e.Actor+":"+e.Kind] = e
		if e.Kind == "fault_window" {
			if window.AtNS != 0 || e.Detail != string(c.Request.Gate) {
				return false
			}
			window = e
		}
	}
	actor := "host"
	if c.Request.Gate == supervisor.BeforeSpawn || c.Request.Gate == supervisor.AfterSpawn {
		actor = "guardian"
	}
	pid := c.HostPID
	if actor == "guardian" {
		pid = c.GuardianPID
	}
	if window.Actor != actor || window.PID != pid || window.AtNS <= 0 || c.FaultAtNS < window.AtNS {
		return false
	}
	has := func(key string) bool { e := events[key]; return e.AtNS > 0 && e.AtNS <= window.AtNS }
	none := func(key string) bool { return events[key].AtNS == 0 }
	switch c.Request.Gate {
	case supervisor.BeforeSpawn:
		return window.TargetPID == 0 && c.TaskPID == 0 && none("guardian:launcher_spawned") && none("launcher:prepared") && none("host:registered") && none("guardian:armed") && none("host:armed")
	case supervisor.AfterSpawn:
		return c.TaskPID > 0 && window.TargetPID == c.TaskPID && has("guardian:launcher_spawned") && none("launcher:prepared") && none("host:registered") && none("guardian:armed") && none("host:armed")
	case supervisor.Registered:
		return window.TargetPID == c.TaskPID && has("guardian:launcher_spawned") && has("host:launcher_observed") && has("launcher:prepared") && has("host:registered") && none("guardian:armed") && none("host:armed")
	case supervisor.Armed:
		return window.TargetPID == c.TaskPID && has("guardian:launcher_spawned") && has("host:launcher_observed") && has("launcher:prepared") && has("host:registered") && has("guardian:armed") && has("host:armed")
	}
	return false
}
