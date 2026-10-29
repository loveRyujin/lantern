//go:build darwin

package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/loveRyujin/lantern/internal/supervisor"
)

func resourceAPICLI(args []string) int {
	f := flag.NewFlagSet("resources-api", flag.ContinueOnError)
	var out, counter string
	f.StringVar(&out, "out", "", "fresh artifact directory")
	f.StringVar(&counter, "counter-report", "", "actual same-source native counter report")
	if f.Parse(args) != nil || f.NArg() != 0 || out == "" || counter == "" {
		return 1
	}
	out, err := filepath.Abs(out)
	if err != nil {
		return 1
	}
	r := resourceAPIReport{Schema: resourceAPISchema, Status: "failed", Cases: []resourceAPICase{}, Fingerprints: map[string]string{}}
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
	r.Environment = map[string]string{"goos": runtime.GOOS, "goarch": runtime.GOARCH, "go_version": runtime.Version(), "macos_version": commandText(ctx, "/usr/bin/sw_vers", "-productVersion"), "macos_build": commandText(ctx, "/usr/bin/sw_vers", "-buildVersion"), "scope": "fixed Go helper; sampled current-task CPU and RSS; no business backend"}
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
	for _, name := range []string{"common.go", "protocol.go", "runtime_darwin.go", "spawn_darwin.go", "resources.go", "resources_darwin.go", "resource_worker_darwin.go"} {
		b, e := os.ReadFile(filepath.Join("internal", "supervisor", name))
		if e != nil {
			r.Error = e.Error()
			return emit()
		}
		r.Fingerprints[name] = digest(b)
	}
	counterFile, e := os.Open(counter)
	if e != nil {
		r.Error = e.Error()
		return emit()
	}
	b, e = io.ReadAll(io.LimitReader(counterFile, 65537))
	counterFile.Close()
	if e != nil || len(b) > 65536 {
		r.Error = "counter report unavailable or over64KiB"
		return emit()
	}
	var cp struct {
		Schema string `json:"schema"`
		Status string `json:"status"`
		Source string `json:"source_commit"`
		Dirty  bool   `json:"source_dirty"`
		Same   bool   `json:"same_pid_birth_image"`
		Burn   uint64 `json:"measured_preexec_burn_cpu_ns"`
		Exit   int    `json:"actual_child_exit_code"`
	}
	if json.Unmarshal(b, &cp) != nil || cp.Schema != "lantern.darwin-resource-counter.v1" || cp.Status != "passed" || cp.Source != r.Source || cp.Dirty != r.Dirty || !cp.Same || cp.Burn < uint64(200*time.Millisecond) || cp.Exit != 0 {
		r.Error = "counter report does not bind actual source and positive control"
		return emit()
	}
	r.CounterSource = cp.Source
	r.CounterSHA = digest(b)
	r.Status = "passed"
	for _, spec := range resourceAPISpecs() {
		c := runResourceAPICase(ctx, out, spec)
		r.Cases = append(r.Cases, c)
		if c.Status != "passed" {
			r.Status = "failed"
		}
	}
	return emit()
}

func runResourceAPICase(parent context.Context, root string, spec resourceAPISpec) (c resourceAPICase) {
	c = resourceAPICase{ID: spec.ID, Status: "failed", Request: spec.Request, ExpectedReason: spec.WantReason, Checks: map[string]bool{}}
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
	c.ErrorKinds = resourceErrorKinds(err)
	if err != nil {
		c.Error = err.Error()
	}
	c.Checks = resourceCaseChecks(c, os.Getpid())
	c.Checks["touch_contract"] = spec.Request.Mode != supervisor.TouchMemory || result.Worker.MappedBytes == 96<<20 && result.Worker.TouchedBytes > 0 && result.Worker.TouchedBytes <= result.Worker.MappedBytes
	c.Checks["untouched_contract"] = spec.Request.Mode != supervisor.UntouchedMemory || result.Worker.MappedBytes == 256<<20 && result.Worker.TouchedBytes == 0 && result.Usage.PeakVirtualBytes >= 256<<20 && result.Usage.PeakRSSBytes < result.Budget.RSSBytes
	c.Status = "passed"
	for name, pass := range c.Checks {
		if !pass {
			c.Status = "failed"
			c.ValidationError = fmt.Sprintf("check failed: %s", name)
			break
		}
	}
	_ = writeJSON(filepath.Join(dir, "case.json"), c)
	return
}
