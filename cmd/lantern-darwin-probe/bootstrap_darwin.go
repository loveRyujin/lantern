//go:build darwin

package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// BootstrapTrial is diagnostic evidence only. It never replaces the baseline
// policy, turns its failed assertion green, or enables the subsequent stages.
// Every payload is still the same fixed bounded vmem experiment.
type BootstrapTrial struct {
	ID            string     `json:"id"`
	Targets       []string   `json:"requested_literal_directories"`
	Granted       []string   `json:"granted_literal_directories"`
	PolicyPath    string     `json:"policy_path,omitempty"`
	PolicySHA256  string     `json:"policy_sha256,omitempty"`
	Execution     *Execution `json:"execution,omitempty"`
	TaskRootClean bool       `json:"task_root_clean"`
	Error         string     `json:"error,omitempty"`
	Scope         string     `json:"scope"`
}

func (x *experiment) bootstrapTrials() []BootstrapTrial {
	candidates := []string{"/", "/System", "/System/Library", "/usr", "/Library"}
	// These are fixed structural parents of the validated configured CPython,
	// not arbitrary user supplied read roots. Each grant is directory-literal.
	for _, suffix := range []string{"", "bin", "Resources", "Resources/Python.app", "Resources/Python.app/Contents", "Resources/Python.app/Contents/MacOS"} {
		candidates = append(candidates, filepath.Join(x.rt.Base.Prefix, suffix))
	}
	seen := map[string]bool{}
	fixed := []string{}
	for _, p := range candidates {
		if !seen[p] {
			seen[p] = true
			fixed = append(fixed, p)
		}
	}
	trials := make([]BootstrapTrial, 0, len(fixed))
	for i, p := range fixed {
		trials = append(trials, x.bootstrapTrial(fmt.Sprintf("literal-%02d", i), []string{p}))
	}
	return trials
}

func (x *experiment) bootstrapTrial(id string, targets []string) BootstrapTrial {
	t := BootstrapTrial{ID: id, Targets: append([]string(nil), targets...), Scope: "diagnostic-only exact directory open/enumeration; no child-content, write, network or general exec grant; never a fallback"}
	plan := x.plan
	plan.OpenDirectories = append([]string(nil), x.plan.OpenDirectories...)
	grants := map[string]bool{}
	for _, p := range targets {
		st, e := os.Stat(p)
		if e != nil || !st.IsDir() {
			t.Error = fmt.Sprintf("required diagnostic target is not an existing directory: %s (%v)", p, e)
			return t
		}
		grants[p] = true
		grants[canonical(p)] = true
	}
	t.Granted = listSorted(grants)
	plan.OpenDirectories = append(plan.OpenDirectories, t.Granted...)
	c, e := x.task("vmem", true, x.exe)
	if e != nil {
		t.Error = e.Error()
		return t
	}
	c.Budget.WallMS = 2000
	c.Budget.CPUSeconds = 1
	c.Budget.CPURLimit = 2
	c.Budget.RSSBytes = 256 << 20
	c.Budget.DiskBytes = 8 << 20
	c.Policy = filepath.Join(x.out, "policies", "bootstrap-"+id+".sb")
	policy := []byte(makePolicy(plan, c, x.exe))
	t.PolicyPath = c.Policy
	t.PolicySHA256 = digest(policy)
	if e = os.WriteFile(c.Policy, policy, 0600); e != nil {
		t.Error = e.Error()
		t.TaskRootClean = x.cleanTasks(c.Root)
		return t
	}
	run := runFixed(x.ctx, x.exe, c, x.monitor)
	t.Execution = &run
	t.TaskRootClean = x.cleanTasks(c.Root)
	if !t.TaskRootClean {
		t.Error = "diagnostic task root cleanup failed"
	}
	return t
}
