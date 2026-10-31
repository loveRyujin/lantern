//go:build darwin

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/loveRyujin/lantern/internal/supervisor"
)

func fileAPICLI(args []string) int {
	f := flag.NewFlagSet("files-api", flag.ContinueOnError)
	var out string
	f.StringVar(&out, "out", "", "fresh artifact directory")
	if f.Parse(args) != nil || f.NArg() != 0 || out == "" {
		return 1
	}
	out, err := filepath.Abs(out)
	if err != nil {
		return 1
	}
	r := fileAPIReport{
		Schema: fileAPISchema, Status: "failed", Cases: []fileAPICase{}, Fingerprints: map[string]string{},
		Algorithm: supervisor.FileBudgetAlgorithm, Candidate: supervisor.FileBudgetCandidate,
		Axes: map[string]string{
			"logical_named_bytes":  "sum of st_size for every regular directory entry; a hard link is counted again and this is not the budget",
			"logical_unique_bytes": "sum of st_size once per regular inode",
			"allocated_bytes":      "candidate budget: sum of st_blocks*512 once per regular inode, lstat only",
			"entries":              "live directory entries under the private root, excluding the root; a separate name ceiling, not the byte budget",
		},
		CleanupIsBudgetProtection: false, HardQuota: false, BusinessBackend: false, HarnessRescue: false,
	}
	emit := func() int {
		if e := writeJSON(filepath.Join(out, "report.json"), r); e != nil {
			r.Status = "failed"
			r.Error = "write report: " + e.Error()
		}
		_ = json.NewEncoder(os.Stdout).Encode(r)
		if r.Status == "passed" {
			return 0
		}
		return 1
	}
	if err = os.Mkdir(out, 0700); err != nil {
		r.Error = "refuse existing or unavailable output: " + err.Error()
		_ = json.NewEncoder(os.Stdout).Encode(r)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	head, e1 := exec.CommandContext(ctx, "git", "rev-parse", "HEAD").Output()
	dirty, e2 := exec.CommandContext(ctx, "git", "status", "--porcelain").Output()
	r.Source = strings.TrimSpace(string(head))
	_, e3 := hex.DecodeString(r.Source)
	r.Dirty = len(dirty) > 0
	r.Environment = map[string]string{
		"goos": runtime.GOOS, "goarch": runtime.GOARCH, "go_version": runtime.Version(),
		"macos_version": commandText(ctx, "/usr/bin/sw_vers", "-productVersion"),
		"macos_build":   commandText(ctx, "/usr/bin/sw_vers", "-buildVersion"),
		"scope":         "sealed Go helper; private work root; lstat allocated-byte candidate; unlink-open unresolved",
	}
	if e1 != nil || e2 != nil || e3 != nil || len(r.Source) != 40 || r.Environment["macos_version"] == "" || r.Environment["macos_build"] == "" {
		r.Error = "native environment or source metadata unavailable"
		return emit()
	}
	exe, e := os.Executable()
	if e != nil {
		r.Error = e.Error()
		return emit()
	}
	b, e := os.ReadFile(exe)
	if e != nil {
		r.Error = e.Error()
		return emit()
	}
	r.Fingerprints["executable_sha256"] = digest(b)
	sum := sha256.Sum256([]byte(supervisor.FileBudgetAlgorithm))
	r.Fingerprints["algorithm_sha256"] = hex.EncodeToString(sum[:])
	for _, name := range []string{"common.go", "protocol.go", "runtime_darwin.go", "spawn_darwin.go", "resources.go", "resources_darwin.go", "resource_worker_darwin.go", "filebudget.go", "filebudget_unix.go", "file_worker_darwin.go"} {
		b, e := os.ReadFile(filepath.Join("internal", "supervisor", name))
		if e != nil {
			r.Error = e.Error()
			return emit()
		}
		r.Fingerprints[name] = digest(b)
	}
	r.Status = "passed"
	r.CleanupActor = "guardian"
	for _, spec := range fileAPISpecs() {
		c := runFileAPICase(ctx, out, spec)
		r.Cases = append(r.Cases, c)
		if c.Status != "passed" {
			r.Status = "failed"
		}
		if !c.Result.WorkRootClean {
			r.CleanupActor = "not-cleaned"
		}
		if c.HarnessRescue {
			r.HarnessRescue = true
		}
		if spec.ID == "file-unlink" {
			r.UnlinkedOpen = c.Conclusion
			r.CandidateMetersUnlinkedOpen = c.Conclusion != "unresolved-unlinked-open" || c.BudgetProtectedDuringRun
		}
	}
	if r.UnlinkedOpen != "unresolved-unlinked-open" || r.CandidateMetersUnlinkedOpen || r.CleanupIsBudgetProtection || r.HarnessRescue || r.CleanupActor != "guardian" {
		r.Status = "failed"
	}
	return emit()
}

func runFileAPICase(parent context.Context, root string, spec fileAPISpec) (c fileAPICase) {
	c = fileAPICase{ID: spec.ID, Status: "failed", Request: spec.Request, ExpectedReason: spec.WantReason, ExpectedAxis: spec.WantAxis, ExpectedConclusion: spec.WantResult, HarnessRescue: false, Checks: map[string]bool{}}
	dir := filepath.Join(root, spec.ID)
	work := filepath.Join(dir, "work")
	evidence := filepath.Join(dir, "evidence")
	if e := os.MkdirAll(work, 0700); e != nil {
		c.ValidationError = e.Error()
		return
	}
	if e := os.Mkdir(evidence, 0700); e != nil {
		c.ValidationError = e.Error()
		return
	}
	ctx, cancel := context.WithTimeout(parent, 7*time.Second)
	defer cancel()
	start := time.Now()
	result, err := supervisor.New(supervisor.Config{TempRoot: work, EvidenceRoot: evidence}).Run(ctx, spec.Request)
	c.WallMS = time.Since(start).Milliseconds()
	c.Result = result
	c.ErrorKinds = fileErrorKinds(err)
	if err != nil {
		c.Error = err.Error()
	}
	c.Conclusion, c.BudgetProtectedDuringRun = fileConclusion(result)
	c.Checks = fileCaseChecks(c, os.Getpid())
	c.Status = "passed"
	for name, pass := range c.Checks {
		if !pass {
			c.Status = "failed"
			c.ValidationError = "check failed: " + name
			break
		}
	}
	_ = writeJSON(filepath.Join(dir, "case.json"), c)
	return
}
