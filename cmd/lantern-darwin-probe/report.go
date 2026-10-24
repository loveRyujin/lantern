package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/loveRyujin/lantern/core/pyrt"
)

const schemaVersion = "lantern.darwin-spike.v1"

var requiredStages = []string{"preflight", "filesystem", "network", "process-profile", "resources", "lifecycle", "sql-worker", "signed-path"}

var requiredChecks = map[string][]string{
	"preflight":       {"prepared_runtime", "sandbox_exec", "environment", "payload_identity", "readonly_plan"},
	"filesystem":      {"host_controls", "input_read", "runtime_read", "out_write", "tmp_write", "host_read_denied", "database_read_denied", "input_write_denied", "host_write_denied", "runtime_write_denied", "symlink_read_denied", "sibling_read_denied", "actual_targets", "payload_completed", "waited", "temp_clean"},
	"network":         {"host_controls", "tcp_denied", "udp_denied", "unix_denied", "payload_completed", "waited", "temp_clean"},
	"process-profile": {"host_controls", "all_attempted", "payload_completed", "waited", "temp_clean"},
	"resources":       {"cpu_limit", "rss_limit", "vmem_positive", "wall_limit", "stdout_limit", "stderr_limit", "disk_limit", "sampling", "waited", "temp_clean"},
	"lifecycle":       {"normal", "error", "cancel", "timeout", "app_death", "guardian_death_observed", "rescue_separate", "waited", "temp_clean"},
	"sql-worker":      {"query_result", "sampled", "waited", "temp_clean"},
	"signed-path":     {"bundle_signed", "signature_verified", "entitlements_recorded", "bundle_path_executed", "benign_png", "fs_denied", "network_denied", "waited", "temp_clean"},
}

var requiredFingerprints = []string{"payload_sha256", "runtime_marker_sha256", "executable_sha256", "policy_sha256", "signed_policy_sha256"}

// Status describes experiment validity, not production backend availability.
// Exploratory observations can establish a negative feasibility result without
// weakening the assertions declared in Contracts before execution.
type Report struct {
	Schema       string            `json:"schema"`
	Status       string            `json:"status"`
	Feasibility  string            `json:"feasibility"`
	Production   bool              `json:"production_backend_available"`
	Started      time.Time         `json:"started"`
	Finished     time.Time         `json:"finished"`
	Environment  map[string]string `json:"environment"`
	Runtime      pyrt.Runtime      `json:"runtime"`
	Fingerprints map[string]string `json:"fingerprints"`
	Stages       []Stage           `json:"stages"`
	Limitations  []string          `json:"limitations"`
	Error        string            `json:"error,omitempty"`
}

type Stage struct {
	ID           string                 `json:"id"`
	Status       string                 `json:"status"`
	Executed     bool                   `json:"executed"`
	Contracts    []string               `json:"assertion_contracts"`
	Exploratory  []string               `json:"exploratory_capabilities,omitempty"`
	Checks       []Check                `json:"checks"`
	Runs         []Execution            `json:"runs,omitempty"`
	Observations []Observation          `json:"observations,omitempty"`
	Details      map[string]interface{} `json:"details,omitempty"`
	Limitations  []string               `json:"limitations,omitempty"`
}

type Check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

type Observation struct {
	Operation string `json:"operation"`
	Attempted bool   `json:"attempted"`
	Success   bool   `json:"success"`
	Errno     int    `json:"errno"`
	Detail    string `json:"detail,omitempty"`
}

type Usage struct {
	Scope          string  `json:"scope"`
	Samples        int     `json:"samples"`
	PeakRSS        uint64  `json:"peak_rss_bytes"`
	PeakVirtual    uint64  `json:"peak_virtual_bytes"`
	CPUSeconds     float64 `json:"cpu_seconds"`
	WallSeconds    float64 `json:"wall_seconds"`
	MaxSampleGapMS float64 `json:"max_sample_gap_ms"`
	KillLatencyMS  float64 `json:"kill_to_wait_ms"`
	PeakFiles      int     `json:"peak_file_entries"`
	PeakDiskBytes  int64   `json:"peak_disk_bytes"`
	ReadErrors     int     `json:"resource_read_errors"`
	Waited         bool    `json:"waited"`
}

type Budget struct {
	CPUSeconds   float64 `json:"sampled_cpu_seconds"`
	WallMS       int     `json:"wall_ms"`
	RSSBytes     uint64  `json:"sampled_rss_bytes"`
	OutputBytes  int     `json:"output_bytes_per_stream"`
	DiskBytes    int64   `json:"sampled_disk_bytes"`
	CPURLimit    uint64  `json:"rlimit_cpu_seconds"`
	FileRLimit   uint64  `json:"rlimit_fsize_bytes"`
	NoFileRLimit uint64  `json:"rlimit_nofile"`
}

type Execution struct {
	Mode           string        `json:"mode"`
	Sandboxed      bool          `json:"sandboxed"`
	Entered        bool          `json:"entered"`
	PID            int           `json:"pid"`
	PayloadPID     int           `json:"payload_pid"`
	ExitCode       int           `json:"exit_code"`
	Signal         string        `json:"signal,omitempty"`
	StopReason     string        `json:"stop_reason,omitempty"`
	Stdout         string        `json:"stdout"`
	Stderr         string        `json:"stderr"`
	StdoutBytes    int64         `json:"stdout_bytes_observed"`
	StderrBytes    int64         `json:"stderr_bytes_observed"`
	Observations   []Observation `json:"observations,omitempty"`
	ObservedImages []string      `json:"observed_images,omitempty"`
	Usage          Usage         `json:"usage"`
	Budget         Budget        `json:"budget"`
	Error          string        `json:"error,omitempty"`
}

func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// Success means the trusted fixture path was observed absent, rather than just
// trusting a deletion helper's return value. Retained artifact paths are separate.
func cleanupFixtureRoot(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		return fmt.Errorf("fixture still present or unobservable after cleanup: %s (%v)", path, err)
	}
	return nil
}

func newReport() Report {
	r := Report{Schema: schemaVersion, Status: "unsupported", Feasibility: "unresolved", Started: time.Now().UTC(),
		Environment:  map[string]string{"os": runtime.GOOS, "arch": runtime.GOARCH, "go": runtime.Version()},
		Fingerprints: map[string]string{}, Limitations: []string{
			"Fixed bounded experiments only; production Darwin Sandbox.Run and SQL worker remain unavailable.",
			"Custom SBPL is deprecated/undocumented for third parties; results apply only to the recorded OS and architecture.",
			"Prepared marker/lock provenance is not attestation of every installed package byte.",
		}, Stages: []Stage{}}
	for _, id := range requiredStages {
		r.Stages = append(r.Stages, Stage{ID: id, Status: "not_run", Checks: []Check{}})
	}
	return r
}

func (r *Report) stage(s Stage) {
	for i := range r.Stages {
		if r.Stages[i].ID == s.ID {
			r.Stages[i] = s
			return
		}
	}
	panic("unregistered stage")
}
func (s *Stage) check(name string, pass bool, detail string) {
	s.Checks = append(s.Checks, Check{name, pass, detail})
	if !pass {
		s.Status = "failed"
	}
}
func stageStart(id string, contracts, exploratory []string) Stage {
	return Stage{ID: id, Status: "passed", Executed: true, Contracts: contracts, Exploratory: exploratory, Checks: []Check{}}
}

func finalize(r *Report) {
	r.Finished = time.Now().UTC()
	if r.Error != "" {
		if r.Status != "unsupported" {
			r.Status = "failed"
		}
		return
	}
	r.Status = "passed"
	if r.Schema != schemaVersion || r.Production || (r.Feasibility != "viable" && r.Feasibility != "unresolved") {
		r.Status = "failed"
	}
	for key, want := range map[string]string{"control_fixture_cleanup": "passed", "runtime_sentinel_cleanup": "passed", "signal_target_waited": "true"} {
		if r.Environment[key] != want {
			r.Status = "failed"
		}
	}
	for _, key := range requiredFingerprints {
		b, e := hex.DecodeString(r.Fingerprints[key])
		if e != nil || len(b) != 32 {
			r.Status = "failed"
		}
	}
	seen := map[string]bool{}
	for _, s := range r.Stages {
		if seen[s.ID] || !s.Executed || s.Status != "passed" || len(s.Contracts) == 0 || len(s.Checks) == 0 {
			r.Status = "failed"
		}
		seen[s.ID] = true
		checks := map[string]bool{}
		for _, c := range s.Checks {
			if !c.Passed || checks[c.Name] {
				r.Status = "failed"
			}
			checks[c.Name] = true
		}
		want, ok := requiredChecks[s.ID]
		if !ok || len(want) != len(checks) {
			r.Status = "failed"
		}
		for _, name := range want {
			if !checks[name] {
				r.Status = "failed"
			}
		}
	}
	if len(seen) != len(requiredStages) {
		r.Status = "failed"
	}
	for _, id := range requiredStages {
		if !seen[id] {
			r.Status = "failed"
		}
	}
}

func exitCode(r Report, require bool) int {
	if r.Status == "unsupported" {
		return 2
	}
	if r.Status != "passed" || (require && r.Feasibility != "viable") {
		return 1
	}
	return 0
}

func writeReports(out string, r Report) error {
	if err := os.MkdirAll(out, 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(out, "report.json"), append(b, '\n'), 0600); err != nil {
		return err
	}
	var md strings.Builder
	fmt.Fprintf(&md, "# Darwin feasibility spike\n\nExperiment: **%s**; feasibility: **%s**; production backend: **false**.\n\n", r.Status, r.Feasibility)
	for _, s := range r.Stages {
		fmt.Fprintf(&md, "## %s — %s\n\nExecuted: %t.\n\n", s.ID, s.Status, s.Executed)
		for _, c := range s.Checks {
			fmt.Fprintf(&md, "- %t — %s: %s\n", c.Passed, c.Name, c.Detail)
		}
		for _, l := range s.Limitations {
			fmt.Fprintf(&md, "- Limitation: %s\n", l)
		}
		md.WriteString("\n")
	}
	for _, l := range r.Limitations {
		fmt.Fprintf(&md, "- %s\n", l)
	}
	if r.Error != "" {
		fmt.Fprintf(&md, "\nError: %s\n", r.Error)
	}
	return os.WriteFile(filepath.Join(out, "report.md"), []byte(md.String()), 0600)
}
