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

func TestDarwinFileBudgetNativeIndependentQA(t *testing.T) {
	if os.Getenv("DARWIN_FILE_MUTATION_QA") != "1" {
		t.Skip("requires explicit real file-budget mutation gate")
	}
	probe := qaNativeRequiredEnv(t, "DARWIN_FILE_PROBE")
	baselinePath := qaNativeRequiredEnv(t, "DARWIN_FILE_BASELINE_REPORT")
	repo := strings.TrimSpace(qaNativeCommand(t, "", "git", "rev-parse", "--show-toplevel"))
	head := strings.TrimSpace(qaNativeCommand(t, repo, "git", "rev-parse", "HEAD"))
	var baseline fileAPIReport
	qaAPIReadJSON(t, baselinePath, &baseline)
	qaFileNativeBaseline(t, baseline, head, repo, probe)
	out := filepath.Join(filepath.Dir(baselinePath), "independent-qa")
	if err := os.Mkdir(out, 0700); err != nil {
		t.Fatal("file independent evidence must be new: ", err)
	}
	type record struct {
		Name         string `json:"name"`
		SourceFile   string `json:"source_file"`
		OriginalSHA  string `json:"original_sha256"`
		MutatedSHA   string `json:"mutated_sha256"`
		ReportPath   string `json:"report_path"`
		TargetCase   string `json:"target_case"`
		ExitScope    string `json:"exit_scope"`
		Exit         int    `json:"exit_code"`
		ObservedExit int    `json:"observed_worker_exit_code"`
		TargetRed    bool   `json:"target_red"`
		Restored     bool   `json:"source_restored"`
	}
	results := []record{}
	index := map[string]any{"schema": "lantern.darwin-file-budget-mutations.v1", "candidate_sha": head, "baseline_status": baseline.Status, "baseline_report": baselinePath, "baseline_source_commit": baseline.Source}
	if prHead := strings.TrimSpace(os.Getenv("LANTERN_PR_HEAD")); len(prHead) == 40 {
		index["pr_head"] = prHead
	}
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
	qaNativeCommand(t, snapshot, "git", "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-c", "user.name=Lantern QA", "-c", "user.email=qa@localhost", "commit", "--quiet", "-m", "isolated file budget QA snapshot")
	snapshotSHA := strings.TrimSpace(qaNativeCommand(t, snapshot, "git", "rev-parse", "HEAD"))
	index["snapshot_commit"] = snapshotSHA
	rec := record{Name: "allocated-stop-disabled", SourceFile: "internal/supervisor/resources_darwin.go", TargetCase: "file-grow", ExitScope: "files-api-cli-oracle"}
	old, newText := "if s.Files.Allocated > r.budget.FileBytes {", "if false && s.Files.Allocated > r.budget.FileBytes {"
	path := filepath.Join(snapshot, rec.SourceFile)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(original), old) != 1 {
		t.Fatal("actual file candidate stop must match exactly once")
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
	dir := filepath.Join(out, rec.Name)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "probe")
	qaFileCommand(t, snapshot, filepath.Join(dir, "build.log"), "go", "build", "-o", binary, "./cmd/lantern-darwin-probe")
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "files-api", "--out", filepath.Join(dir, "run"))
	cmd.Dir = snapshot
	b, err := cmd.CombinedOutput()
	_ = os.WriteFile(filepath.Join(dir, "command.log"), b, 0600)
	if ctx.Err() != nil {
		t.Fatal("outer timeout is not file budget evidence")
	}
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 1 {
		t.Fatalf("actual file oracle did not reject the removed candidate stop: %v %s", err, b)
	}
	rec.Exit = exit.ExitCode()
	rec.ReportPath = filepath.Join(dir, "run", "report.json")
	var report fileAPIReport
	qaAPIReadJSON(t, rec.ReportPath, &report)
	if report.Schema != fileAPISchema || report.Status != "failed" || report.Error != "" || report.Source != snapshotSHA || !report.Dirty || len(report.Cases) != 6 {
		t.Fatal("mutant did not actually run the six same-source file cases")
	}
	byID := map[string]*fileAPICase{}
	for i := range report.Cases {
		byID[report.Cases[i].ID] = &report.Cases[i]
	}
	for _, id := range []string{"file-small", "file-entries", "file-unlink", "file-edges"} {
		c := byID[id]
		if c == nil || c.Status != "passed" {
			t.Fatalf("unrelated file case changed under the allocated-stop mutation: %s", id)
		}
	}
	if byID["file-entries"].Result.StopReason != "file_limit" || byID["file-entries"].Result.Usage.FileTriggerAxis != "entries" {
		t.Fatal("entry ceiling was removed along with the candidate byte stop")
	}
	for _, id := range []string{"file-grow", "file-multi"} {
		qaFileAssertRemovedStop(t, *byID[id])
	}
	rec.ObservedExit = byID["file-grow"].Result.Worker.ExitCode
	rec.TargetRed = true
}

func qaFileCommand(t *testing.T, dir, log, name string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	b, err := cmd.CombinedOutput()
	if e := os.WriteFile(log, b, 0600); e != nil {
		t.Fatal(e)
	}
	if err != nil || ctx.Err() != nil {
		t.Fatalf("build failure is not file-budget mutation evidence: %v %s", err, b)
	}
}

func qaFileNativeBaseline(t *testing.T, r fileAPIReport, head, repo, probe string) {
	t.Helper()
	if r.Schema != fileAPISchema || r.Status != "passed" || r.Source != head || r.Dirty || r.Error != "" || r.BusinessBackend || r.HardQuota || r.HarnessRescue || r.CleanupIsBudgetProtection || r.CandidateMetersUnlinkedOpen || r.UnlinkedOpen != "unresolved-unlinked-open" || r.Candidate != supervisor.FileBudgetCandidate || r.CleanupActor != "guardian" || len(r.Cases) != 6 || r.Environment["goos"] != "darwin" || r.Environment["goarch"] != "arm64" {
		t.Fatal("complete real six-case file baseline is required")
	}
	b, err := os.ReadFile(probe)
	if err != nil || digest(b) != r.Fingerprints["executable_sha256"] {
		t.Fatal("native binary provenance mismatch")
	}
	for _, file := range []string{"common.go", "protocol.go", "runtime_darwin.go", "spawn_darwin.go", "resources.go", "resources_darwin.go", "resource_worker_darwin.go", "filebudget.go", "filebudget_unix.go", "filebudget_darwin.go", "file_worker_darwin.go"} {
		b, err := os.ReadFile(filepath.Join(repo, "internal", "supervisor", file))
		if err != nil || digest(b) != r.Fingerprints[file] {
			t.Fatal("file source fingerprint mismatch: ", file)
		}
	}
	want := map[string]bool{}
	for _, s := range fileAPISpecs() {
		want[s.ID] = true
	}
	for _, c := range r.Cases {
		if !want[c.ID] || c.Status != "passed" || c.ValidationError != "" || c.HarnessRescue || c.BudgetProtectedDuringRun {
			t.Fatal("native file manifest missing or rescued: ", c.ID)
		}
		delete(want, c.ID)
		for _, pass := range c.Checks {
			if !pass {
				t.Fatal("native file baseline has a false assertion: ", c.ID)
			}
		}
		for name, pass := range fileCaseChecks(c, c.Result.HostPID) {
			if !pass {
				t.Fatal("independent replay rejects file baseline: ", c.ID, name)
			}
		}
	}
	if len(want) != 0 {
		t.Fatal("file cases missing")
	}
}

func qaFileAssertRemovedStop(t *testing.T, c fileAPICase) {
	t.Helper()
	r, u := c.Result, c.Result.Usage
	if c.Status != "failed" || c.Error != "" || len(c.ErrorKinds) != 0 || r.State != supervisor.Completed || r.Worker.ExitCode != 0 || r.Worker.Signal != 0 || r.StopReason != "" || u.TriggerReason != "" || u.FileTriggerAxis != "" || !r.Worker.Entered || !r.HostWaitedGuardian || !r.GuardianWaitedTask || !r.TaskAbsent || !r.GuardianAbsent || !r.WorkRootClean || c.HarnessRescue || c.WallMS >= 6000 {
		t.Fatalf("mutation red was not a naturally completed helper past its file budget: %+v", c)
	}
	if u.PeakFileAllocated <= r.Budget.FileBytes || r.Budget.Wall <= u.Wall || u.PeakFileAllocated == 0 {
		t.Fatal("no raw allocated-byte breach inside the wall budget was observed")
	}
	for _, s := range u.Samples {
		if s.Valid && s.Phase == "running" && s.MonotonicNS >= u.GoMonotonicNS && s.MonotonicNS <= u.WaitMonotonicNS && (s.CPU >= r.Budget.CPU || s.RSSBytes > r.Budget.RSSBytes) {
			t.Fatal("outer CPU or RSS budget is what the removed file stop is hiding")
		}
	}
}
