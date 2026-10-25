//go:build darwin

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/loveRyujin/lantern/internal/supervisor"
)

// The disposable acceptance host calls Run in its own process. It is the host,
// not a subprocess invented by the production API to stand in for its caller.
type apiHostReturn struct {
	Returned bool              `json:"returned"`
	HostPID  int               `json:"host_pid"`
	Result   supervisor.Result `json:"result"`
	Error    string            `json:"error,omitempty"`
}
type apiCase struct {
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
	Schema        string    `json:"schema"`
	Status        string    `json:"status"`
	Source        string    `json:"source_commit"`
	Dirty         bool      `json:"source_dirty"`
	ExecutableSHA string    `json:"executable_sha256"`
	Cases         []apiCase `json:"cases"`
	Error         string    `json:"error,omitempty"`
}

func supervisorAPIInternal(args []string) (bool, int) {
	if len(args) == 0 || args[0] != "__supervisor-api-host" {
		return false, 0
	}
	if len(args) != 5 {
		return true, 125
	}
	var q supervisor.Request
	if json.Unmarshal([]byte(args[3]), &q) != nil {
		return true, 125
	}
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	if args[4] == "cancel" {
		go func() {
			until := time.Now().Add(5 * time.Second)
			for time.Now().Before(until) {
				for _, e := range apiEvents(args[2]) {
					if e.Kind == "payload_entered" {
						cancel()
						return
					}
				}
				time.Sleep(time.Millisecond)
			}
		}()
	}
	result, err := supervisor.New(supervisor.Config{TempRoot: args[1], EvidenceRoot: args[2]}).Run(ctx, q)
	r := apiHostReturn{Returned: true, HostPID: os.Getpid(), Result: result}
	if err != nil {
		r.Error = err.Error()
	}
	if writeJSON(filepath.Join(args[2], "return.json"), r) != nil {
		return true, 125
	}
	return true, 0
}
func apiEvents(root string) []supervisor.Event {
	var out []supervisor.Event
	files, _ := filepath.Glob(filepath.Join(root, "supervisor-evidence-*", "*.jsonl"))
	for _, name := range files {
		f, e := os.Open(name)
		if e != nil {
			continue
		}
		s := bufio.NewScanner(f)
		for s.Scan() {
			var v supervisor.Event
			if json.Unmarshal(s.Bytes(), &v) == nil {
				out = append(out, v)
			}
		}
		f.Close()
	}
	return out
}
func apiAbsent(pid int) bool { return pid <= 0 || syscall.Kill(pid, 0) == syscall.ESRCH }
func supervisorAPICLI(args []string) int {
	f := flag.NewFlagSet("supervision-api", flag.ContinueOnError)
	var out string
	f.StringVar(&out, "out", "", "fresh acceptance directory")
	if f.Parse(args) != nil || f.NArg() != 0 || out == "" {
		return 1
	}
	out, _ = filepath.Abs(out)
	r := apiReport{Schema: "lantern.darwin-supervisor-api.v1", Status: "failed", Cases: []apiCase{}}
	// Exclusive ownership before starting any actor or inspecting old evidence.
	if e := os.Mkdir(out, 0700); e != nil {
		r.Error = e.Error()
		json.NewEncoder(os.Stdout).Encode(r)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	r.Source = commandText(ctx, "git", "rev-parse", "HEAD")
	r.Dirty = commandText(ctx, "git", "status", "--porcelain") != ""
	exe, e := os.Executable()
	if e != nil {
		r.Error = e.Error()
	} else {
		b, err := os.ReadFile(exe)
		if err != nil {
			r.Error = err.Error()
		} else {
			r.ExecutableSHA = digest(b)
		}
		for _, gate := range []supervisor.Gate{supervisor.BeforeSpawn, supervisor.AfterSpawn, supervisor.Registered, supervisor.Armed} {
			for _, target := range []string{"host", "guardian"} {
				r.Cases = append(r.Cases, runAPICase(ctx, exe, out, "startup-"+string(gate)+"-"+target, supervisor.Request{Kind: supervisor.GoLifecycle, Mode: supervisor.Hold, Gate: gate}, target, false))
			}
		}
		for _, mode := range []supervisor.Mode{supervisor.Hold, supervisor.SetSID, supervisor.SetPGID} {
			label := string(mode)
			if mode == supervisor.Hold {
				label = "steady"
			}
			for _, target := range []string{"host", "guardian"} {
				r.Cases = append(r.Cases, runAPICase(ctx, exe, out, label+"-"+target, supervisor.Request{Kind: supervisor.GoLifecycle, Mode: mode}, target, false))
			}
		}
		for _, mode := range []supervisor.Mode{supervisor.Normal, supervisor.ExpectedError, supervisor.Hold, supervisor.ExitRace} {
			label := string(mode)
			cancelCase := mode == supervisor.Hold || mode == supervisor.ExitRace
			if mode == supervisor.Hold {
				label = "cancel"
			}
			r.Cases = append(r.Cases, runAPICase(ctx, exe, out, label, supervisor.Request{Kind: supervisor.GoLifecycle, Mode: mode}, "", cancelCase))
		}
	}
	if r.Error == "" && len(r.Cases) == 18 {
		r.Status = "passed"
		for _, c := range r.Cases {
			if c.Status != "passed" {
				r.Status = "failed"
			}
		}
	}
	if e := writeJSON(filepath.Join(out, "report.json"), r); e != nil {
		r.Status = "failed"
		r.Error = e.Error()
	}
	json.NewEncoder(os.Stdout).Encode(r)
	if r.Status != "passed" {
		return 1
	}
	return 0
}

func runAPICase(parent context.Context, exe, root, id string, q supervisor.Request, target string, cancelCase bool) (c apiCase) {
	started := time.Now()
	c = apiCase{ID: id, Status: "failed", Request: q, FaultTarget: target, Checks: map[string]bool{}}
	defer func() { c.WallMS = time.Since(started).Milliseconds() }()
	dir := filepath.Join(root, id)
	work := filepath.Join(dir, "work")
	evidence := filepath.Join(dir, "evidence")
	if e := os.MkdirAll(work, 0700); e != nil {
		c.Error = e.Error()
		return
	}
	if e := os.Mkdir(evidence, 0700); e != nil {
		c.Error = e.Error()
		return
	}
	arg, _ := json.Marshal(q)
	action := "none"
	if cancelCase {
		action = "cancel"
	}
	cmd := exec.Command(exe, "__supervisor-api-host", work, evidence, string(arg), action)
	log, e := os.OpenFile(filepath.Join(dir, "host-output.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		c.Error = e.Error()
		return
	}
	defer log.Close()
	cmd.Stdout = log
	cmd.Stderr = log
	if e := cmd.Start(); e != nil {
		c.Error = e.Error()
		return
	}
	c.HostPID = cmd.Process.Pid
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	waited := false
	faulted := false
	var faultTime time.Time
	identities := map[int]ProcessIdentity{}
	monitor, err := newProcessMonitor()
	if err != nil {
		c.Error = err.Error()
	}
	until := time.Now().Add(8 * time.Second)
	for time.Now().Before(until) && parent.Err() == nil {
		c.Events = apiEvents(evidence)
		checkpoint := false
		for _, v := range c.Events {
			if v.Kind == "guardian_started" {
				c.GuardianPID = v.TargetPID
			}
			if v.Kind == "launcher_spawned" || v.Kind == "launcher_observed" || v.Kind == "registered" {
				c.TaskPID = v.TargetPID
			}
			if q.Gate != "" && v.Kind == "fault_window" && v.Detail == string(q.Gate) {
				checkpoint = true
			}
			if q.Gate == "" && v.Kind == "payload_entered" {
				checkpoint = true
			}
		}
		if monitor != nil {
			for _, pid := range []int{c.HostPID, c.GuardianPID, c.TaskPID} {
				if pid > 0 {
					if _, ok := identities[pid]; !ok {
						if v, e := monitor.identity(pid); e == nil {
							identities[pid] = v
						}
					}
				}
			}
		}
		if target != "" && checkpoint && !faulted {
			if monitor == nil {
				c.Error = "process identity monitor unavailable"
				break
			}
			pid := c.HostPID
			if target == "guardian" {
				pid = c.GuardianPID
			}
			// Signal only a live identity observed by this acceptance run.
			old, ok := identities[pid]
			now, e := monitor.identity(pid)
			if !ok || e != nil || old != now {
				c.Error = "fault identity unavailable or changed"
				break
			}
			if e = syscall.Kill(pid, syscall.SIGKILL); e != nil {
				c.Error = e.Error()
				break
			}
			faulted = true
			faultTime = time.Now()
		}
		if !waited {
			select {
			case <-done:
				waited = true
			default:
			}
		}
		if waited && apiAbsent(c.GuardianPID) && apiAbsent(c.TaskPID) {
			break
		}
		if faulted && time.Since(faultTime) > 2*time.Second {
			c.Error = "fault response exceeded two seconds"
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	c.Events = apiEvents(evidence)
	b, _ := os.ReadFile(filepath.Join(evidence, "return.json"))
	_ = json.Unmarshal(b, &c.Return)
	files, _ := filepath.Glob(filepath.Join(evidence, "supervisor-evidence-*", "guardian-receipt.json"))
	if len(files) == 1 {
		c.GuardianReceipt, _ = os.ReadFile(files[0])
	}
	absent := waited && apiAbsent(c.HostPID) && apiAbsent(c.GuardianPID) && apiAbsent(c.TaskPID)
	entered, goSeen, registered, armed := false, false, false, false
	for _, v := range c.Events {
		switch v.Kind {
		case "payload_entered":
			entered = true
		case "go_authorized":
			goSeen = true
		case "registered":
			registered = true
		case "armed":
			armed = true
		}
	}
	c.Checks["real_host"] = !c.Return.Returned || c.Return.HostPID == c.HostPID && c.Return.Result.HostPID == c.HostPID
	c.Checks["fault_injected"] = target == "" || faulted
	c.Checks["processes_absent"] = absent
	c.Checks["go_gate"] = !entered || goSeen && registered && armed
	c.Checks["startup_no_payload"] = q.Gate == "" || !entered && !goSeen
	c.Checks["host_result"] = target == "host" && !c.Return.Returned || target != "host" && c.Return.Returned
	if target == "host" {
		var receipt struct {
			Waited, Absent, Clean bool
			Error                 string
		}
		_ = json.Unmarshal(c.GuardianReceipt, &receipt)
		c.Checks["cleanup"] = receipt.Clean && receipt.Absent && receipt.Error == "" && (q.Gate == supervisor.BeforeSpawn || receipt.Waited)
	} else {
		r := c.Return.Result
		c.Checks["cleanup"] = r.WorkRootClean && r.TaskAbsent && r.GuardianAbsent && r.HostWaitedGuardian
		switch {
		case target == "guardian":
			c.Checks["outcome"] = r.State == supervisor.Failed && c.Return.Error != ""
		case cancelCase:
			c.Checks["outcome"] = r.State == supervisor.Cancelled && strings.Contains(c.Return.Error, "canceled") || q.Mode == supervisor.ExitRace && r.State == supervisor.Completed && r.Worker.ExitCode == 0
		default:
			want := 0
			if q.Mode == supervisor.ExpectedError {
				want = 1
			}
			c.Checks["outcome"] = r.State == supervisor.Completed && c.Return.Error == "" && r.Worker.ExitCode == want && r.Worker.Entered
		}
	}
	c.Checks["detachment"] = true
	if q.Mode == supervisor.SetSID || q.Mode == supervisor.SetPGID {
		w := c.Return.Result.Worker
		if target == "host" {
			var receipt struct {
				Worker supervisor.WorkerResult `json:"worker"`
			}
			_ = json.Unmarshal(c.GuardianReceipt, &receipt)
			w = receipt.Worker
		}
		c.Checks["detachment"] = w.Entered && w.Group == w.PID && (q.Mode != supervisor.SetSID || w.Session == w.PID)
	}
	if !absent {
		c.HarnessRescue = true
		for _, pid := range []int{c.TaskPID, c.GuardianPID, c.HostPID} {
			old, ok := identities[pid]
			if monitor != nil && ok {
				now, e := monitor.identity(pid)
				if e == nil && now == old {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			}
		}
		if !waited {
			select {
			case <-done:
			case <-time.After(time.Second):
			}
		}
	}
	c.Checks["no_harness_rescue"] = !c.HarnessRescue
	c.Checks["deadline"] = time.Since(started) < 8*time.Second
	if c.Error == "" {
		c.Status = "passed"
		for name, ok := range c.Checks {
			if !ok {
				c.Status = "failed"
				c.Error = fmt.Sprintf("check failed: %s", name)
				break
			}
		}
	}
	_ = writeJSON(filepath.Join(dir, "case.json"), c)
	return
}
