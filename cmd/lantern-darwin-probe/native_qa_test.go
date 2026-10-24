//go:build darwin

package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// This test is separate from the portable report/CLI contract tests. CI must
// supply an already prepared runtime and a complete native baseline; it never
// installs dependencies and never treats a failed preflight as a killed mutant.
func TestDarwinSpikeNativeIndependentQA(t *testing.T) {
	if os.Getenv("DARWIN_SPIKE_REAL_QA") != "1" {
		t.Skip("native independent experiments require DARWIN_SPIKE_REAL_QA=1")
	}
	probe := qaNativeRequiredEnv(t, "DARWIN_SPIKE_PROBE")
	runtimeConfig := qaNativeRequiredEnv(t, "DARWIN_SPIKE_RUNTIME_CONFIG")
	baselinePath := qaNativeRequiredEnv(t, "DARWIN_SPIKE_BASELINE_REPORT")
	if info, err := os.Stat(probe); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("required native probe missing: %v", err)
	}
	prepared, err := readRuntime(runtimeConfig)
	if err != nil {
		t.Fatalf("required prepared runtime config: %v", err)
	}
	repo := strings.TrimSpace(qaNativeCommand(t, "", "git", "rev-parse", "--show-toplevel"))
	head := strings.TrimSpace(qaNativeCommand(t, repo, "git", "rev-parse", "HEAD"))
	baseline := qaNativeReadReport(t, baselinePath)
	qaNativeAssertBaseline(t, baseline, head)
	probeBytes, err := os.ReadFile(probe)
	if err != nil || baseline.Fingerprints["executable_sha256"] != digest(probeBytes) || !reflect.DeepEqual(prepared, baseline.Runtime) {
		t.Fatalf("baseline did not use the supplied actual binary and prepared runtime: %v", err)
	}
	evidence := filepath.Join(filepath.Dir(baselinePath), "independent-qa")
	if err := os.MkdirAll(evidence, 0700); err != nil {
		t.Fatal(err)
	}
	// Signatures are rechecked after the baseline has finished, through the
	// actual retained bundle rather than a stand-alone signed executable.
	bundle, _ := qaNativeStage(t, baseline, "signed-path").Details["bundle"].(string)
	if bundle == "" {
		t.Fatal("native signed bundle path absent")
	}
	qaNativeCommand(t, "", "/usr/bin/codesign", "--verify", "--deep", "--strict", bundle)
	payload, err := os.ReadFile(filepath.Join(bundle, "Contents", "Resources", "payload.py"))
	if err != nil || !bytes.Equal(payload, pythonPayload) {
		t.Fatalf("signed bundle does not contain the actual fixed payload: %v", err)
	}
	if !pngValid(filepath.Join(filepath.Dir(baselinePath), "signed", "plot.png")) {
		t.Fatal("retained actual signed-path PNG is invalid")
	}

	snapshot := t.TempDir()
	qaNativeArchive(t, repo, snapshot)
	qaNativeCommand(t, snapshot, "git", "init", "--quiet")
	qaNativeCommand(t, snapshot, "git", "add", "--all", "--force")
	qaNativeCommand(t, snapshot, "git", "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-c", "user.name=Lantern QA", "-c", "user.email=qa@localhost", "commit", "--quiet", "-m", "isolated native QA source snapshot")
	snapshotSHA := strings.TrimSpace(qaNativeCommand(t, snapshot, "git", "rev-parse", "HEAD"))
	mutations := []struct {
		name, file, old, replacement, stage, check string
	}{
		{"network-allow", "policy_darwin.go", `b.WriteString("(deny network*)\n(deny process-fork)\n")`, `b.WriteString("(allow network*)\n(deny process-fork)\n")`, "network", "tcp_denied"},
		{"host-read-allow", "policy_darwin.go", `return b.String()`, `fmt.Fprintf(&b, "(allow file-read* (literal %s))\n", strconv.Quote(c.Outside)); return b.String()`, "filesystem", "host_read_denied"},
		{"host-udp-control-broken", "probe_darwin.go", `string(b[:n]) == "lantern-udp"`, `string(b[:n]) == "intentionally-invalid-control-token"`, "network", "host_controls"},
		{"directory-cleanup-noop", "report.go", `if err := os.RemoveAll(path); err != nil {`, `if err := func() error { if s,e := os.Lstat(path); e == nil && s.IsDir() { return nil }; return os.RemoveAll(path) }(); err != nil {`, "filesystem", "temp_clean"},
		{"guardian-eof-disabled", "lifecycle_darwin.go", `go func() { _, _ = os.Stdin.Read(make([]byte, 1)); cancel() }()`, `go func() { _, _ = os.Stdin.Read(make([]byte, 1)) }()`, "lifecycle", "app_death"},
		{"rss-monitor-disabled", "execute_darwin.go", `if t.Resident > c.Budget.RSSBytes {`, `if false && t.Resident > c.Budget.RSSBytes {`, "resources", "rss_limit"},
		{"cpu-monitor-disabled", "execute_darwin.go", `if r.Usage.CPUSeconds > c.Budget.CPUSeconds {`, `if false && r.Usage.CPUSeconds > c.Budget.CPUSeconds {`, "resources", "cpu_limit"},
	}
	type record struct {
		Name           string `json:"name"`
		SourceFile     string `json:"source_file"`
		OriginalSHA256 string `json:"original_sha256"`
		MutatedSHA256  string `json:"mutated_sha256"`
		ReportPath     string `json:"report_path"`
		ExitCode       int    `json:"exit_code"`
		TargetRed      bool   `json:"target_red"`
		SourceRestored bool   `json:"source_restored"`
	}
	records := []record{}
	defer func() {
		b, _ := json.MarshalIndent(map[string]interface{}{"candidate_sha": head, "snapshot_commit": snapshotSHA, "baseline_report": baselinePath, "baseline_status": baseline.Status, "platform": "actual native Darwin", "results": records}, "", "  ")
		_ = os.WriteFile(filepath.Join(evidence, "index.json"), append(b, '\n'), 0600)
	}()
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			path := filepath.Join(snapshot, "cmd", "lantern-darwin-probe", mutation.file)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(original), mutation.old) != 1 {
				t.Fatalf("mutation must match exactly one actual source statement: %s", mutation.file)
			}
			changed := []byte(strings.Replace(string(original), mutation.old, mutation.replacement, 1))
			if err := os.WriteFile(path, changed, 0600); err != nil {
				t.Fatal(err)
			}
			rec := record{Name: mutation.name, SourceFile: mutation.file, OriginalSHA256: digest(original), MutatedSHA256: digest(changed)}
			defer func() {
				if err := os.WriteFile(path, original, 0600); err != nil {
					t.Error(err)
				}
				got, err := os.ReadFile(path)
				rec.SourceRestored = err == nil && bytes.Equal(got, original)
				if !rec.SourceRestored {
					t.Error("isolated mutant source did not restore")
				}
				records = append(records, rec)
			}()
			out := filepath.Join(evidence, mutation.name)
			if err := os.MkdirAll(out, 0700); err != nil {
				t.Fatal(err)
			}
			executable := filepath.Join(t.TempDir(), "probe")
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			build := exec.CommandContext(ctx, "go", "build", "-o", executable, "./cmd/lantern-darwin-probe")
			build.Dir = snapshot
			build.Env = append(os.Environ(), "CGO_ENABLED=0")
			body, err := build.CombinedOutput()
			cancel()
			_ = os.WriteFile(filepath.Join(out, "build.log"), body, 0600)
			if err != nil {
				t.Fatalf("mutant build failure is not target evidence: %v %s", err, body)
			}
			ctx, cancel = context.WithTimeout(t.Context(), 5*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "--runtime-config", runtimeConfig, "--out", out)
			cmd.Dir = snapshot
			body, err = cmd.CombinedOutput()
			_ = os.WriteFile(filepath.Join(out, "command.log"), body, 0600)
			if ctx.Err() != nil {
				t.Fatal("mutant outer timeout is not target evidence")
			}
			code := 0
			if err != nil {
				exit, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatalf("mutant did not execute: %v", err)
				}
				code = exit.ExitCode()
			}
			rec.ExitCode = code
			rec.ReportPath = filepath.Join(out, "report.json")
			r := qaNativeReadReport(t, rec.ReportPath)
			defer qaNativeCleanupOwnedFixture(t, r)
			if code != 1 || r.Status != "failed" || r.Production {
				t.Fatalf("mutated protection/control was not rejected: exit=%d status=%s error=%s", code, r.Status, r.Error)
			}
			if r.Environment["source_commit"] != snapshotSHA || r.Environment["source_dirty"] != "true" {
				t.Fatal("mutant report did not bind its actual dirty source snapshot")
			}
			pre := qaNativeStage(t, r, "preflight")
			if pre.Status != "passed" || !pre.Executed {
				t.Fatalf("preflight failure cannot kill this mutation: %+v", pre.Checks)
			}
			stage := qaNativeStage(t, r, mutation.stage)
			if stage.Status != "failed" || qaNativeCheck(t, stage, mutation.check) {
				t.Fatalf("missing required target assertion failure: %s/%s", mutation.stage, mutation.check)
			}
			qaNativeAssertMutantBehavior(t, mutation.name, stage, r)
			rec.TargetRed = true
			t.Logf("actual behavior killed %s: %s/%s; evidence=%s", mutation.name, mutation.stage, mutation.check, out)
		})
	}
}

func qaNativeRequiredEnv(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" || !filepath.IsAbs(value) {
		t.Fatalf("required native QA path %s missing or not absolute", name)
	}
	return value
}

func qaNativeReadReport(t *testing.T, path string) Report {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var r Report
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("actual report is not complete JSON: %v", err)
	}
	return r
}

func qaNativeStage(t *testing.T, r Report, id string) Stage {
	t.Helper()
	for _, s := range r.Stages {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("missing actual stage %s", id)
	return Stage{}
}

func qaNativeCheck(t *testing.T, s Stage, name string) bool {
	t.Helper()
	for _, c := range s.Checks {
		if c.Name == name {
			return c.Passed
		}
	}
	t.Fatalf("missing actual check %s/%s", s.ID, name)
	return false
}

func qaNativeRun(t *testing.T, s Stage, mode string, sandboxed bool) Execution {
	t.Helper()
	for _, r := range s.Runs {
		if r.Mode == mode && r.Sandboxed == sandboxed {
			return r
		}
	}
	t.Fatalf("missing actual run %s sandboxed=%t", mode, sandboxed)
	return Execution{}
}

func qaNativeAssertBaseline(t *testing.T, r Report, head string) {
	t.Helper()
	if r.Schema != "lantern.darwin-spike.v1" || r.Status != "passed" || r.Production || (r.Feasibility != "viable" && r.Feasibility != "unresolved") {
		t.Fatalf("required complete baseline is unavailable: status=%s error=%s", r.Status, r.Error)
	}
	if r.Environment["os"] != "darwin" || r.Environment["source_commit"] != head || r.Environment["source_dirty"] != "false" || len(r.Stages) != len(qaRequiredStages) {
		t.Fatal("baseline platform/source/exact stage manifest mismatch")
	}
	for key, want := range map[string]string{"control_fixture_cleanup": "passed", "runtime_sentinel_cleanup": "passed", "signal_target_waited": "true"} {
		if r.Environment[key] != want {
			t.Fatalf("baseline cleanup evidence missing: %s", key)
		}
	}
	for _, key := range []string{"payload_sha256", "runtime_marker_sha256", "executable_sha256", "policy_sha256", "signed_policy_sha256"} {
		b, err := hex.DecodeString(r.Fingerprints[key])
		if err != nil || len(b) != 32 {
			t.Fatalf("baseline fingerprint missing: %s", key)
		}
	}
	if r.Fingerprints["payload_sha256"] != digest(pythonPayload) {
		t.Fatal("baseline actual payload differs from this source snapshot")
	}
	seen := map[string]bool{}
	for _, s := range r.Stages {
		if seen[s.ID] || !s.Executed || s.Status != "passed" || len(s.Contracts) == 0 {
			t.Fatalf("invalid baseline stage: %+v", s)
		}
		seen[s.ID] = true
		want, ok := qaRequiredChecks[s.ID]
		if !ok || len(s.Checks) != len(want) {
			t.Fatalf("baseline check set mismatch: %s", s.ID)
		}
		for _, name := range want {
			if !qaNativeCheck(t, s, name) {
				t.Fatalf("baseline assertion failed: %s/%s", s.ID, name)
			}
		}
		for _, run := range s.Runs {
			if !run.Entered || run.PID != run.PayloadPID || !run.Usage.Waited {
				t.Fatalf("baseline claims an unentered or unawaited payload in %s: %+v", s.ID, run)
			}
		}
	}
	for _, id := range qaRequiredStages {
		if !seen[id] {
			t.Fatalf("baseline stage missing: %s", id)
		}
	}
}

func qaNativeAssertMutantBehavior(t *testing.T, name string, stage Stage, r Report) {
	t.Helper()
	switch name {
	case "network-allow":
		run := qaNativeRun(t, stage, "network", true)
		if !goodRun(run) || !opSuccess(run, "tcp_connect") || !opSuccess(run, "udp_exchange") || !opSuccess(run, "unix_connect") {
			t.Fatal("network mutation did not actually reach all controlled host endpoints")
		}
	case "host-read-allow":
		run := qaNativeRun(t, stage, "filesystem", true)
		if !goodRun(run) || !opSuccess(run, "host_read") {
			t.Fatal("filesystem mutation did not actually read the owned host sentinel")
		}
	case "host-udp-control-broken":
		run := qaNativeRun(t, stage, "network", false)
		o, ok := findOp(run, "udp_exchange")
		if !run.Entered || !run.Usage.Waited || !ok || !o.Attempted || o.Success || o.Errno != 0 {
			t.Fatal("broken UDP listener control did not fail as an actual host timeout observation")
		}
	case "directory-cleanup-noop":
		root := r.Environment["control_fixture_root"]
		if r.Environment["control_fixture_cleanup"] != "failed" {
			t.Fatal("no-op fixture cleanup did not reach final report")
		}
		if info, err := os.Lstat(root); err != nil || !info.IsDir() {
			t.Fatal("no-op deletion did not leave an actual owned fixture to detect")
		}
	case "guardian-eof-disabled":
		var o deathObservation
		b, _ := json.Marshal(stage.Details["app_death"])
		if err := json.Unmarshal(b, &o); err != nil || o.Error != "" || !o.PayloadEntered || !o.VictimWaited || o.MechanismCleanup || !o.PythonPresentBeforeRescue || !o.HarnessRescue || !o.PythonAbsentAfterRescue {
			t.Fatalf("EOF mutation did not produce the actual observed orphan followed by labelled rescue: %+v error=%v", o, err)
		}
	case "rss-monitor-disabled":
		run := qaNativeRun(t, stage, "rss", true)
		if !goodRun(run) || run.Usage.PeakRSS <= run.Budget.RSSBytes {
			t.Fatal("RSS mutation did not allow the entered over-budget payload to finish")
		}
	case "cpu-monitor-disabled":
		run := qaNativeRun(t, stage, "cpu", true)
		if !run.Entered || !run.Usage.Waited || run.StopReason != "" || run.Usage.CPUSeconds <= run.Budget.CPUSeconds || run.Signal == "" {
			t.Fatal("CPU mutation did not reach the actual integer-second rlimit backstop")
		}
	}
}

func qaNativeCleanupOwnedFixture(t *testing.T, r Report) {
	t.Helper()
	root := r.Environment["control_fixture_root"]
	if root == "" {
		return
	}
	// The path is returned by this owned fixed probe, not by a model. Restrict
	// rescue cleanup to its exact mktemp namespace and leave retained evidence.
	if !filepath.IsAbs(root) || !strings.HasPrefix(filepath.Base(root), "lantern-darwin-空格 spike-") {
		t.Errorf("unexpected fixture cleanup path: %q", root)
		return
	}
	if err := cleanupFixtureRoot(root); err != nil {
		t.Error(err)
	}
}

func qaNativeCommand(t *testing.T, dir, program string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Dir = dir
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("native QA prerequisite command %s failed: %v %s", program, err, b)
	}
	return string(b)
}

func qaNativeArchive(t *testing.T, repo, destination string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "archive", "--format=tar", "HEAD")
	cmd.Dir = repo
	data, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	r := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if !filepath.IsLocal(h.Name) {
			t.Fatalf("archive path escape: %q", h.Name)
		}
		p := filepath.Join(destination, h.Name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0700); err != nil {
				t.Fatal(err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
				t.Fatal(err)
			}
			b, err := io.ReadAll(r)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, b, os.FileMode(h.Mode)&0777); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("unsupported archive entry type %d: %s", h.Typeflag, h.Name)
		}
	}
}
