//go:build darwin

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/internal/supervisor"
)

func TestDarwinResourceAPINativeIndependentQA(t *testing.T) {
	if os.Getenv("DARWIN_RESOURCE_MUTATION_QA") != "1" {
		t.Skip("requires explicit real resource mutation gate")
	}
	probe := qaNativeRequiredEnv(t, "DARWIN_RESOURCE_PROBE")
	baselinePath := qaNativeRequiredEnv(t, "DARWIN_RESOURCE_BASELINE_REPORT")
	repo := strings.TrimSpace(qaNativeCommand(t, "", "git", "rev-parse", "--show-toplevel"))
	head := strings.TrimSpace(qaNativeCommand(t, repo, "git", "rev-parse", "HEAD"))
	var baseline resourceAPIReport
	qaAPIReadJSON(t, baselinePath, &baseline)
	qaResourceNativeBaseline(t, baseline, head, repo, probe)
	out := filepath.Join(filepath.Dir(baselinePath), "independent-qa")
	if err := os.Mkdir(out, 0700); err != nil {
		t.Fatal("resource independent evidence must be new: ", err)
	}
	type record struct {
		Name          string `json:"name"`
		SourceFile    string `json:"source_file"`
		OriginalSHA   string `json:"original_sha256"`
		MutatedSHA    string `json:"mutated_sha256"`
		CounterReport string `json:"counter_report"`
		CounterSHA    string `json:"counter_report_sha256"`
		ReportPath    string `json:"report_path"`
		TargetCase    string `json:"target_case"`
		ExitScope     string `json:"exit_scope"`
		Exit          int    `json:"exit_code"`
		ObservedExit  int    `json:"observed_worker_exit_code"`
		TargetRed     bool   `json:"target_red"`
		Restored      bool   `json:"source_restored"`
	}
	results := []record{}
	index := map[string]any{"schema": "lantern.darwin-resource-mutations.v1", "candidate_sha": head, "baseline_status": baseline.Status, "baseline_report": baselinePath, "baseline_source_commit": baseline.Source}
	defer func() {
		index["status"] = "passed"
		if t.Failed() {
			index["status"] = "failed"
		}
		index["results"] = results
		if err := writeJSON(filepath.Join(out, "index.json"), index); err != nil {
			t.Error(err)
		}
	}()
	snapshot := t.TempDir()
	qaNativeArchive(t, repo, snapshot)
	qaNativeCommand(t, snapshot, "git", "init", "--quiet")
	qaNativeCommand(t, snapshot, "git", "add", "--all", "--force")
	qaNativeCommand(t, snapshot, "git", "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-c", "user.name=Lantern QA", "-c", "user.email=qa@localhost", "commit", "--quiet", "-m", "isolated resource policy QA snapshot")
	snapshotSHA := strings.TrimSpace(qaNativeCommand(t, snapshot, "git", "rev-parse", "HEAD"))
	index["snapshot_commit"] = snapshotSHA
	for _, name := range []string{"cpu-monitor-disabled", "rss-monitor-disabled", "wall-monitor-disabled"} {
		t.Run(name, func(t *testing.T) {
			rec := record{Name: name, SourceFile: "internal/supervisor/resources_darwin.go", ExitScope: "resources-api-cli-oracle"}
			rec.TargetCase = "cpu-limit"
			old, newText := "if s.CPU > r.budget.CPU {", "if false && s.CPU > r.budget.CPU {"
			if name == "rss-monitor-disabled" {
				rec.TargetCase = "rss-limit"
				old, newText = "if s.RSSBytes > r.budget.RSSBytes {", "if false && s.RSSBytes > r.budget.RSSBytes {"
			}
			if name == "wall-monitor-disabled" {
				rec.TargetCase = "wall-limit"
				rec.SourceFile = "internal/supervisor/runtime_darwin.go"
				old, newText = "resourceStop(\"wall_limit\", ErrWallLimit)", "_ = meter // independent QA removes online wall STOP only"
			}
			path := filepath.Join(snapshot, rec.SourceFile)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(original), old) != 1 {
				t.Fatal("actual resource safeguard must match exactly once: ", name)
			}
			changed := strings.Replace(string(original), old, newText, 1)
			rec.OriginalSHA, rec.MutatedSHA = digest(original), digest([]byte(changed))
			if err := os.WriteFile(path, []byte(changed), 0600); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := os.WriteFile(path, original, 0600); err != nil {
					t.Error(err)
				}
				b, err := os.ReadFile(path)
				rec.Restored = err == nil && digest(b) == rec.OriginalSHA
				if !rec.Restored {
					t.Error("isolated source restoration failed")
				}
				results = append(results, rec)
			}()
			dir := filepath.Join(out, name)
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			binary := filepath.Join(t.TempDir(), "probe")
			qaResourceCommand(t, snapshot, filepath.Join(dir, "build.log"), nil, "go", "build", "-o", binary, "./cmd/lantern-darwin-probe")
			// Counter provenance is measured anew under this actual dirty source.
			// Never copy/relabel the original clean baseline's report.
			counterDir := filepath.Join(dir, "counter")
			qaResourceCommand(t, snapshot, filepath.Join(dir, "counter.log"), []string{"DARWIN_RESOURCE_COUNTER_QA=1", "DARWIN_RESOURCE_COUNTER_OUT=" + counterDir}, "go", "test", "./internal/supervisor", "-run", "^TestResourceCounterNativeQA$", "-count=1", "-v")
			rec.CounterReport = filepath.Join(counterDir, "report.json")
			counter, err := os.ReadFile(rec.CounterReport)
			if err != nil {
				t.Fatal(err)
			}
			rec.CounterSHA = digest(counter)
			var cp struct {
				Status string `json:"status"`
				Source string `json:"source_commit"`
				Dirty  bool   `json:"source_dirty"`
			}
			qaAPIReadJSON(t, rec.CounterReport, &cp)
			if cp.Status != "passed" || cp.Source != snapshotSHA || !cp.Dirty {
				t.Fatal("counter preflight failure cannot count as resource mutation red")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 75*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, "resources-api", "--counter-report", rec.CounterReport, "--out", filepath.Join(dir, "run"))
			cmd.Dir = snapshot
			b, err := cmd.CombinedOutput()
			_ = os.WriteFile(filepath.Join(dir, "command.log"), b, 0600)
			if ctx.Err() != nil {
				t.Fatal("outer timeout is not resource policy evidence")
			}
			exit, ok := err.(*exec.ExitError)
			if !ok || exit.ExitCode() != 1 {
				t.Fatalf("actual budget oracle did not reject removed safeguard: %v %s", err, b)
			}
			rec.Exit = exit.ExitCode()
			rec.ReportPath = filepath.Join(dir, "run", "report.json")
			var report resourceAPIReport
			qaAPIReadJSON(t, rec.ReportPath, &report)
			if report.Schema != resourceAPISchema || report.Status != "failed" || report.Error != "" || report.Source != snapshotSHA || !report.Dirty || report.CounterSource != snapshotSHA || report.CounterSHA != rec.CounterSHA || len(report.Cases) != 6 {
				t.Fatal("mutant did not actually run the six same-source native resource cases")
			}
			var target *resourceAPICase
			for i := range report.Cases {
				if report.Cases[i].ID == rec.TargetCase {
					target = &report.Cases[i]
				}
			}
			if target == nil {
				t.Fatal("required actual target missing")
			}
			qaResourceAssertRemovedSafeguard(t, *target)
			rec.ObservedExit = target.Result.Worker.ExitCode
			rec.TargetRed = true
			t.Logf("actual resource budget breach detected: %s %s", name, rec.ReportPath)
		})
	}
}

func qaResourceCommand(t *testing.T, dir, log string, extraEnv []string, name string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "CGO_ENABLED=0"), extraEnv...)
	b, err := cmd.CombinedOutput()
	if e := os.WriteFile(log, b, 0600); e != nil {
		t.Fatal(e)
	}
	if err != nil || ctx.Err() != nil {
		t.Fatalf("build/prerequisite failure is not safeguard mutation evidence: %v %s", err, b)
	}
}

func qaResourceNativeBaseline(t *testing.T, r resourceAPIReport, head, repo, probe string) {
	t.Helper()
	if r.Schema != resourceAPISchema || r.Status != "passed" || r.Source != head || r.Dirty || r.Error != "" || r.CounterSource != head || len(r.CounterSHA) != 64 || r.BusinessBackend || r.HardQuota || len(r.Cases) != 6 || r.Environment["goos"] != "darwin" || r.Environment["goarch"] != "arm64" {
		t.Fatal("complete real six-case resource baseline is required")
	}
	b, err := os.ReadFile(probe)
	if err != nil || digest(b) != r.Fingerprints["executable_sha256"] {
		t.Fatal("native binary provenance mismatch")
	}
	for _, file := range []string{"common.go", "protocol.go", "runtime_darwin.go", "spawn_darwin.go", "resources.go", "resources_darwin.go", "resource_worker_darwin.go"} {
		b, err := os.ReadFile(filepath.Join(repo, "internal", "supervisor", file))
		if err != nil || digest(b) != r.Fingerprints[file] {
			t.Fatal("resource source fingerprint mismatch: ", file)
		}
	}
	want := map[string]bool{}
	for _, s := range resourceAPISpecs() {
		want[s.ID] = true
	}
	for _, c := range r.Cases {
		if !want[c.ID] || c.Status != "passed" || c.ValidationError != "" || len(c.Checks) == 0 {
			t.Fatal("native manifest missing/duplicate/incomplete case: ", c.ID)
		}
		delete(want, c.ID)
		for _, pass := range c.Checks {
			if !pass {
				t.Fatal("native baseline has false assertion: ", c.ID)
			}
		}
		for _, pass := range resourceCaseChecks(c, c.Result.HostPID) {
			if !pass {
				t.Fatal("independent replay rejects actual baseline: ", c.ID)
			}
		}
	}
	if len(want) != 0 {
		t.Fatal("resource cases missing")
	}
}

func qaResourceAssertRemovedSafeguard(t *testing.T, c resourceAPICase) {
	t.Helper()
	r, u := c.Result, c.Result.Usage
	if c.Status != "failed" || c.ValidationError == "" || c.Error != "" || len(c.ErrorKinds) != 0 || r.State != supervisor.Completed || r.Worker.ExitCode != 0 || r.Worker.Signal != 0 || r.StopReason != "" || u.TriggerReason != "" || !u.MonitoringReady || !u.MonitoringStarted || !r.Worker.Entered || r.Worker.PID != r.Task.PID || !r.HostWaitedGuardian || !r.GuardianWaitedTask || !r.GuardianAbsent || !r.TaskAbsent || !r.WorkRootClean || c.WallMS >= 7000 {
		t.Fatalf("mutation red was not a naturally completed real helper exceeding its removed budget: %+v", c)
	}
	breach := false
	for _, s := range u.Samples {
		if !s.Valid || s.Phase != "running" || s.PID != r.Task.PID || s.MonotonicNS < u.GoMonotonicNS || s.MonotonicNS > u.WaitMonotonicNS {
			continue
		}
		switch c.ID {
		case "cpu-limit":
			breach = breach || s.CPU > r.Budget.CPU
		case "rss-limit":
			breach = breach || s.RSSBytes > r.Budget.RSSBytes
		}
	}
	if c.ID == "wall-limit" {
		breach = u.Wall > r.Budget.Wall && u.Wall >= 3500*time.Millisecond
	}
	if !breach {
		t.Fatal("no raw on-target budget breach was observed")
	}
}
