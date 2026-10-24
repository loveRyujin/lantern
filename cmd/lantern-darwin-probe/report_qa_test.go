package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var qaRequiredStages = []string{"preflight", "filesystem", "network", "process-profile", "resources", "lifecycle", "sql-worker", "signed-path"}

var qaRequiredChecks = map[string][]string{
	"preflight":       {"prepared_runtime", "sandbox_exec", "environment", "payload_identity", "readonly_plan"},
	"filesystem":      {"host_controls", "input_read", "runtime_read", "out_write", "tmp_write", "host_read_denied", "database_read_denied", "input_write_denied", "host_write_denied", "runtime_write_denied", "symlink_read_denied", "sibling_read_denied", "actual_targets", "payload_completed", "waited", "temp_clean"},
	"network":         {"host_controls", "tcp_denied", "udp_denied", "unix_denied", "payload_completed", "waited", "temp_clean"},
	"process-profile": {"host_controls", "all_attempted", "payload_completed", "waited", "temp_clean"},
	"resources":       {"cpu_limit", "rss_limit", "vmem_positive", "wall_limit", "stdout_limit", "stderr_limit", "disk_limit", "sampling", "waited", "temp_clean"},
	"lifecycle":       {"normal", "error", "cancel", "timeout", "app_death", "guardian_death_observed", "rescue_separate", "waited", "temp_clean"},
	"sql-worker":      {"query_result", "sampled", "waited", "temp_clean"},
	"signed-path":     {"bundle_signed", "signature_verified", "entitlements_recorded", "bundle_path_executed", "benign_png", "fs_denied", "network_denied", "waited", "temp_clean"},
}

func qaCompletedReport() Report {
	r := newReport()
	r.Status = "passed"
	r.Environment["control_fixture_cleanup"] = "passed"
	r.Environment["runtime_sentinel_cleanup"] = "passed"
	r.Environment["signal_target_waited"] = "true"
	for _, name := range []string{"payload_sha256", "runtime_marker_sha256", "executable_sha256", "policy_sha256", "signed_policy_sha256"} {
		r.Fingerprints[name] = strings.Repeat("ab", 32)
	}
	for i, id := range qaRequiredStages {
		r.Stages[i] = Stage{ID: id, Status: "passed", Executed: true,
			Contracts:   []string{"fixed assertion declared before execution"},
			Exploratory: []string{"capability may be absent without weakening the assertion"},
			Checks:      []Check{}}
		for _, name := range qaRequiredChecks[id] {
			r.Stages[i].Checks = append(r.Stages[i].Checks, Check{Name: name, Passed: true})
		}
	}
	return r
}

func TestDarwinProbeQANewReportHasNoExecutionClaims(t *testing.T) {
	r := newReport()
	if r.Schema != "lantern.darwin-spike.v1" || r.Production || r.Status != "unsupported" || r.Feasibility != "unresolved" {
		t.Fatalf("new report claims unsupported execution: %+v", r)
	}
	var ids []string
	for _, stage := range r.Stages {
		ids = append(ids, stage.ID)
		if stage.Executed || stage.Status != "not_run" || len(stage.Checks) != 0 {
			t.Fatalf("new stage claims observations: %+v", stage)
		}
	}
	if !reflect.DeepEqual(ids, qaRequiredStages) || exitCode(r, false) != 2 {
		t.Fatalf("required stages or unsupported exit changed: ids=%v exit=%d", ids, exitCode(r, false))
	}
}

func TestDarwinProbeQAExperimentAndFeasibilityAreSeparate(t *testing.T) {
	r := qaCompletedReport()
	r.Stages[5].Limitations = []string{"guardian death requires harness rescue; not an independent cleanup guarantee"}
	r.Stages[5].Observations = []Observation{{Operation: "guardian-death-self-cleanup", Attempted: true, Success: false}}
	finalize(&r)
	if r.Status != "passed" || r.Feasibility != "unresolved" || r.Production || exitCode(r, false) != 0 || exitCode(r, true) != 1 {
		t.Fatalf("valid negative experiment confused with production feasibility: %+v", r)
	}
	if r.Finished.Before(r.Started) || r.Finished.IsZero() {
		t.Fatal("completion timestamp missing")
	}
	r.Feasibility = "viable"
	if exitCode(r, true) != 0 || r.Production {
		t.Fatal("feasibility result must not enable the production backend")
	}
}

func TestDarwinProbeQAIncompleteAndFailedEvidenceCannotPass(t *testing.T) {
	cases := map[string]func(*Report){
		"missing-stage":           func(r *Report) { r.Stages = r.Stages[:7] },
		"extra-stage":             func(r *Report) { r.Stages = append(r.Stages, r.Stages[0]) },
		"duplicate-stage":         func(r *Report) { r.Stages[7].ID = r.Stages[0].ID },
		"unknown-stage":           func(r *Report) { r.Stages[7].ID = "unknown" },
		"unexecuted-stage":        func(r *Report) { r.Stages[2].Executed = false },
		"unsupported-stage":       func(r *Report) { r.Stages[2].Status = "unsupported" },
		"failed-stage":            func(r *Report) { r.Stages[2].Status = "failed" },
		"unknown-status":          func(r *Report) { r.Stages[2].Status = "unknown" },
		"undeclared-contract":     func(r *Report) { r.Stages[2].Contracts = nil },
		"no-checks":               func(r *Report) { r.Stages[2].Checks = nil },
		"missing-check":           func(r *Report) { r.Stages[2].Checks = r.Stages[2].Checks[1:] },
		"unknown-check":           func(r *Report) { r.Stages[2].Checks[0].Name = "unregistered" },
		"duplicate-check":         func(r *Report) { r.Stages[2].Checks[1].Name = r.Stages[2].Checks[0].Name },
		"failed-check":            func(r *Report) { r.Stages[2].Checks[0].Passed = false },
		"missing-fingerprint":     func(r *Report) { delete(r.Fingerprints, "payload_sha256") },
		"short-fingerprint":       func(r *Report) { r.Fingerprints["payload_sha256"] = "abcd" },
		"nonhex-fingerprint":      func(r *Report) { r.Fingerprints["payload_sha256"] = strings.Repeat("z", 64) },
		"error-after-passed":      func(r *Report) { r.Error = "experiment interrupted" },
		"invalid-feasibility":     func(r *Report) { r.Feasibility = "unknown" },
		"production-claim":        func(r *Report) { r.Production = true },
		"unknown-schema":          func(r *Report) { r.Schema = "unknown" },
		"missing-control-cleanup": func(r *Report) { delete(r.Environment, "control_fixture_cleanup") },
		"failed-control-cleanup":  func(r *Report) { r.Environment["control_fixture_cleanup"] = "failed" },
		"missing-runtime-cleanup": func(r *Report) { delete(r.Environment, "runtime_sentinel_cleanup") },
		"failed-runtime-cleanup":  func(r *Report) { r.Environment["runtime_sentinel_cleanup"] = "failed" },
		"missing-signal-wait":     func(r *Report) { delete(r.Environment, "signal_target_waited") },
		"failed-signal-wait":      func(r *Report) { r.Environment["signal_target_waited"] = "false" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := qaCompletedReport()
			mutate(&r)
			finalize(&r)
			if r.Status == "passed" || exitCode(r, false) == 0 {
				t.Fatalf("invalid evidence passed: status=%s feasibility=%s error=%q", r.Status, r.Feasibility, r.Error)
			}
		})
	}
}

func TestDarwinProbeQACleanupRemovesActualFixtures(t *testing.T) {
	for _, kind := range []string{"directory", "file"} {
		t.Run(kind, func(t *testing.T) {
			parent := t.TempDir()
			fixture := filepath.Join(parent, "owned 中文 fixture")
			if kind == "directory" {
				if err := os.MkdirAll(filepath.Join(fixture, "nested"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(fixture, "nested", "sentinel"), []byte("owned"), 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(fixture, []byte("owned"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := cleanupFixtureRoot(fixture); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(fixture); !os.IsNotExist(err) {
				t.Fatalf("fixture remains after successful cleanup: %v", err)
			}
			if _, err := os.Stat(parent); err != nil {
				t.Fatalf("cleanup removed unrequested parent: %v", err)
			}
			if err := cleanupFixtureRoot(fixture); err != nil {
				t.Fatalf("already-absent fixture: %v", err)
			}
		})
	}
}

func TestDarwinProbeQAUnsupportedReportIsPreserved(t *testing.T) {
	r := newReport()
	r.Error = "required platform or runtime unavailable; no payload executed"
	finalize(&r)
	if r.Status != "unsupported" || exitCode(r, false) != 2 || exitCode(r, true) != 2 {
		t.Fatalf("unsupported evidence must not turn into a pass: %+v", r)
	}
	out := filepath.Join(t.TempDir(), "报告 with spaces")
	if err := writeReports(out, r); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(out, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded Report
	if err := json.Unmarshal(b, &decoded); err != nil || decoded.Error != r.Error || decoded.Status != "unsupported" || len(decoded.Stages) != 8 {
		t.Fatalf("failure evidence lost: error=%v report=%+v", err, decoded)
	}
	md, err := os.ReadFile(filepath.Join(out, "report.md"))
	if err != nil || !strings.Contains(string(md), "unresolved") || !strings.Contains(string(md), r.Error) || !strings.Contains(string(md), "production backend: **false**") {
		t.Fatalf("Markdown hides platform/production limitation: %s error=%v", md, err)
	}
}
