package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/loveRyujin/lantern/core/pyrt"
)

const supervisionSchema = "lantern.darwin-supervision.v1"

var supervisionCaseIDs = []string{"startup-before-spawn-app", "startup-before-spawn-guardian", "startup-after-spawn-app", "startup-after-spawn-guardian", "startup-registered-app", "startup-registered-guardian", "startup-armed-app", "startup-armed-guardian", "steady-app", "steady-guardian", "setsid-app", "setsid-guardian", "setpgid-app", "setpgid-guardian", "normal", "error", "cancel", "exit-cancel-race"}
var supervisionCheckIDs = []string{"protocol_events", "go_gate", "payload_contract", "fault_response", "processes_absent", "mechanism_cleanup", "no_harness_rescue", "deadline"}

type SupervisionEvent struct {
	Actor     string `json:"actor"`
	Seq       int    `json:"seq"`
	AtNS      int64  `json:"at_ns"`
	Kind      string `json:"kind"`
	PID       int    `json:"pid"`
	TargetPID int    `json:"target_pid,omitempty"`
	Detail    string `json:"detail,omitempty"`
}
type SupervisionCase struct {
	ID               string                        `json:"id"`
	Status           string                        `json:"status"`
	Window           string                        `json:"window"`
	FaultTarget      string                        `json:"fault_target"`
	Events           []SupervisionEvent            `json:"events"`
	Checks           []Check                       `json:"checks"`
	AppPID           int                           `json:"app_pid"`
	GuardianPID      int                           `json:"guardian_pid"`
	LauncherPID      int                           `json:"launcher_pid"`
	PayloadPID       int                           `json:"payload_pid"`
	Registered       bool                          `json:"registered"`
	Armed            bool                          `json:"armed"`
	GoAuthorized     bool                          `json:"go_authorized"`
	PayloadEntered   bool                          `json:"payload_entered"`
	AppAbsent        bool                          `json:"app_absent"`
	GuardianAbsent   bool                          `json:"guardian_absent"`
	PayloadAbsent    bool                          `json:"payload_absent"`
	MechanismCleanup bool                          `json:"mechanism_cleanup"`
	TaskClean        bool                          `json:"task_clean"`
	HarnessRescue    bool                          `json:"harness_rescue"`
	FaultAtNS        int64                         `json:"fault_at_ns,omitempty"`
	ResponseMS       float64                       `json:"fault_to_absent_ms"`
	WallMS           float64                       `json:"wall_ms"`
	Receipts         map[string]supervisionReceipt `json:"receipts"`
	Payload          *supervisionMarker            `json:"payload,omitempty"`
	Error            string                        `json:"error,omitempty"`
}
type SupervisionReport struct {
	Schema                 string            `json:"schema"`
	Status                 string            `json:"status"`
	Production             bool              `json:"production_backend_available"`
	SingleFaultVerified    bool              `json:"single_fault_verified"`
	FaultModel             string            `json:"fault_model"`
	Environment            map[string]string `json:"environment"`
	Runtime                pyrt.Runtime      `json:"runtime"`
	Fingerprints           map[string]string `json:"fingerprints"`
	Preflight              []Check           `json:"preflight_checks"`
	Cases                  []SupervisionCase `json:"cases"`
	Cleanup                bool              `json:"control_fixture_cleanup"`
	PreserveExistingOutput bool              `json:"existing_output_preserved,omitempty"`
	Limitations            []string          `json:"limitations"`
	Error                  string            `json:"error,omitempty"`
	Started                time.Time         `json:"started"`
	Finished               time.Time         `json:"finished"`
}

// Receipts distinguish a real direct-child Wait from orphan disappearance.
type supervisionReceipt struct {
	Actor            string              `json:"actor"`
	PID              int                 `json:"pid"`
	Target           supervisionIdentity `json:"target"`
	Waited           bool                `json:"waited"`
	WaitedPID        int                 `json:"waited_pid"`
	ExitCode         int                 `json:"exit_code"`
	StopReason       string              `json:"stop_reason,omitempty"`
	TargetAbsent     bool                `json:"target_absent"`
	Cleanup          bool                `json:"cleanup"`
	Registered       bool                `json:"registered"`
	Armed            bool                `json:"armed"`
	Go               bool                `json:"go"`
	Error            string              `json:"error,omitempty"`
	SignalSkipped    bool                `json:"signal_skipped,omitempty"`
	SignalSkipReason string              `json:"signal_skip_reason,omitempty"`
	SignalResolvedBy string              `json:"signal_resolved_by,omitempty"`
}
type supervisionIdentity struct {
	PID               int    `json:"pid"`
	StartSeconds      uint64 `json:"start_seconds"`
	StartMicroseconds uint64 `json:"start_microseconds"`
	Image             string `json:"image"`
	Nonce             string `json:"nonce"`
}
type supervisionMarker struct {
	PID           int    `json:"pid"`
	Mode          string `json:"mode"`
	OpenFDs       []int  `json:"open_fds"`
	StdinEmpty    bool   `json:"stdin_empty"`
	ForkErrno     int    `json:"fork_errno"`
	SpawnErrno    int    `json:"spawn_errno"`
	Detached      bool   `json:"detached"`
	OriginalGroup int    `json:"original_group"`
	Group         int    `json:"group"`
	Session       int    `json:"session"`
}

func newSupervisionReport() SupervisionReport {
	e := newReport().Environment
	return SupervisionReport{Schema: supervisionSchema, Status: "unsupported", Environment: e, Fingerprints: map[string]string{}, Preflight: []Check{}, Cases: []SupervisionCase{}, Started: time.Now().UTC(), FaultModel: "One trusted process exits or is SIGKILLed; the other observer remains alive and schedulable. No simultaneous app+guardian kill, SIGSTOP/hang, reboot or kernel atomic parent-death guarantee.", Limitations: []string{"Fixed bounded payloads only; production Darwin Runner/SQL remain unavailable.", "PID, birth and pre-exec image checks are not pidfd-bound atomic signalling. After GO, monitoring follows the registered PID/birth across exec.", "EOF/kqueue are asynchronous observations. No claim that exec is atomically impossible at the exact instant of peer death.", "Resources remain one direct PID with threads; no process-tree quota or hard instantaneous RSS claim."}}
}

func finalizeSupervision(r *SupervisionReport) {
	r.Finished = time.Now().UTC()
	r.SingleFaultVerified = false
	if r.Error != "" {
		if r.Status != "unsupported" {
			r.Status = "failed"
		}
		return
	}
	r.Status = "passed"
	if r.Schema != supervisionSchema || r.Production || !r.Cleanup || len(r.Cases) != len(supervisionCaseIDs) || len(r.Preflight) == 0 {
		r.Status = "failed"
	}
	pre := map[string]bool{}
	for _, c := range r.Preflight {
		if pre[c.Name] {
			r.Status = "failed"
		}
		pre[c.Name] = true
	}
	if len(pre) != 5 {
		r.Status = "failed"
	}
	for _, key := range []string{"platform", "runtime_prepared", "host_fork_positive", "policy_declared", "source_metadata"} {
		if !pre[key] {
			r.Status = "failed"
		}
	}
	for _, key := range []string{"protocol_sha256", "payload_sha256", "policy_sha256", "runtime_marker_sha256", "executable_sha256"} {
		b, e := hex.DecodeString(r.Fingerprints[key])
		if e != nil || len(b) != 32 {
			r.Status = "failed"
		}
	}
	for _, c := range r.Preflight {
		if !c.Passed {
			r.Status = "failed"
		}
	}
	seen := map[string]bool{}
	for _, c := range r.Cases {
		if validateSupervisionCase(c) != nil {
			r.Status = "failed"
		}
		known := false
		for _, id := range supervisionCaseIDs {
			if c.ID == id {
				known = true
			}
		}
		if !known || seen[c.ID] || c.Status != "passed" || c.Error != "" || c.HarnessRescue || !c.TaskClean || !c.MechanismCleanup || !c.AppAbsent || !c.GuardianAbsent || !c.PayloadAbsent {
			r.Status = "failed"
		}
		seen[c.ID] = true
		checks := map[string]bool{}
		for _, v := range c.Checks {
			if checks[v.Name] || !v.Passed {
				r.Status = "failed"
			}
			checks[v.Name] = true
		}
		if len(checks) != len(supervisionCheckIDs) {
			r.Status = "failed"
		}
		for _, id := range supervisionCheckIDs {
			if !checks[id] {
				r.Status = "failed"
			}
		}
	}
	r.SingleFaultVerified = r.Status == "passed"
}

func supervisionCLI(args []string) int {
	o, e := parseOptions(args)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		return 1
	}
	if o.RequireFeasible {
		fmt.Fprintln(os.Stderr, "supervision has a bounded single-fault contract, not a production-feasible flag")
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	r := runSupervision(ctx, o)
	finalizeSupervision(&r)
	if r.PreserveExistingOutput {
		_ = json.NewEncoder(os.Stdout).Encode(r)
		return 1
	}
	if e = os.MkdirAll(o.Out, 0700); e == nil {
		var b []byte
		b, e = json.MarshalIndent(r, "", "  ")
		if e == nil {
			e = os.WriteFile(filepath.Join(o.Out, "report.json"), append(b, '\n'), 0600)
		}
	}
	if e != nil {
		r.Status = "failed"
		r.SingleFaultVerified = false
		r.Error = "report: " + e.Error()
	}
	if err := writeSupervisionMarkdown(o.Out, r); err != nil {
		r.Status = "failed"
		r.SingleFaultVerified = false
		r.Error = "markdown report: " + err.Error()
		if b, je := json.MarshalIndent(r, "", "  "); je == nil {
			_ = os.WriteFile(filepath.Join(o.Out, "report.json"), append(b, '\n'), 0600)
		}
	}
	_ = json.NewEncoder(os.Stdout).Encode(r)
	if r.Status == "unsupported" {
		return 2
	}
	if r.Status != "passed" {
		return 1
	}
	return 0
}

// Reject stale PID-bearing directories without modifying their contents.
func createSupervisionControl(path string) error { return os.Mkdir(path, 0700) }

func writeSupervisionMarkdown(out string, r SupervisionReport) error {
	body := fmt.Sprintf("# Darwin single-fault supervision\n\nExperiment **%s**; single-fault verified **%t**; production backend **false**.\n\n%s\n\n", r.Status, r.SingleFaultVerified, r.FaultModel)
	for _, c := range r.Cases {
		body += fmt.Sprintf("- %s: %s; entered=%t; mechanism_cleanup=%t; harness_rescue=%t; fault_to_absent=%.2fms\n", c.ID, c.Status, c.PayloadEntered, c.MechanismCleanup, c.HarnessRescue, c.ResponseMS)
	}
	for _, l := range r.Limitations {
		body += "\n- " + l
	}
	if r.Error != "" {
		body += "\n\nError: " + r.Error
	}
	return os.WriteFile(filepath.Join(out, "report.md"), []byte(body+"\n"), 0600)
}
