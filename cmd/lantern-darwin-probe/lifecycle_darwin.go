//go:build darwin

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

type deathReceipt struct {
	GuardianPID      int       `json:"guardian_pid"`
	Execution        Execution `json:"execution"`
	MechanismCleanup bool      `json:"mechanism_cleanup"`
	Error            string    `json:"error,omitempty"`
}
type appReceipt struct {
	AppPID      int `json:"app_pid"`
	GuardianPID int `json:"guardian_pid"`
}
type deathObservation struct {
	Kind                      string        `json:"kind"`
	AppPID                    int           `json:"app_pid"`
	GuardianPID               int           `json:"guardian_pid"`
	PythonPID                 int           `json:"python_pid"`
	PayloadEntered            bool          `json:"payload_entered"`
	VictimWaited              bool          `json:"victim_waited"`
	PythonPresentBeforeRescue bool          `json:"python_present_before_rescue"`
	MechanismCleanup          bool          `json:"mechanism_cleanup"`
	HarnessRescue             bool          `json:"harness_rescue"`
	PythonAbsentAfterRescue   bool          `json:"python_absent_after_rescue"`
	TaskRootClean             bool          `json:"task_root_clean"`
	Receipt                   *deathReceipt `json:"mechanism_receipt,omitempty"`
	Error                     string        `json:"error,omitempty"`
}

func app(path string) int {
	c, e := readTask(path)
	if e != nil {
		return 125
	}
	exe, e := os.Executable()
	if e != nil {
		return 125
	}
	r, w, e := os.Pipe()
	if e != nil {
		return 125
	}
	defer w.Close()
	cmd := exec.Command(exe, "__guardian", path)
	cmd.Stdin = r
	if e = cmd.Start(); e != nil {
		r.Close()
		return 125
	}
	r.Close()
	if e = writeJSON(c.AppReady, appReceipt{os.Getpid(), cmd.Process.Pid}); e != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return 125
	}
	// Deliberately minimal candidate: the app holds only the liveness pipe.
	// Guardian unexpected death is observed without silently adding harness
	// rescue to the mechanism. Its orphaned bounded child is rescued by the
	// outer test harness and recorded as a negative feasibility observation.
	if cmd.Wait() != nil {
		return 1
	}
	return 0
}

func guardian(path string) int {
	c, e := readTask(path)
	if e != nil {
		return 125
	}
	m, e := newProcessMonitor()
	if e != nil {
		return 125
	}
	exe, e := os.Executable()
	if e != nil {
		return 125
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	go func() { _, _ = os.Stdin.Read(make([]byte, 1)); cancel() }()
	run := runFixed(ctx, exe, c, m)
	e = os.RemoveAll(c.Root)
	receipt := deathReceipt{GuardianPID: os.Getpid(), Execution: run, MechanismCleanup: e == nil && run.Usage.Waited}
	if e != nil {
		receipt.Error = e.Error()
	}
	if e = writeJSON(c.Receipt, receipt); e != nil {
		return 1
	}
	if !receipt.MechanismCleanup {
		return 1
	}
	return 0
}

func readJSON(path string, value interface{}) error {
	b, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	if len(b) > 1<<20 {
		return fmt.Errorf("oversized experiment receipt")
	}
	return json.Unmarshal(b, value)
}
func processAbsent(_ *processMonitor, pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == syscall.ESRCH
}
func waitAbsent(ctx context.Context, m *processMonitor, pid int, d time.Duration) bool {
	until := time.Now().Add(d)
	for time.Now().Before(until) {
		if processAbsent(m, pid) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(10 * time.Millisecond):
		}
	}
	return processAbsent(m, pid)
}

// Only a still-live process whose executable matches the fixed prepared Python
// is signalled. The harness never scans/kills unrelated host process groups.
func rescuePython(m *processMonitor, pid int, python string) bool {
	if processAbsent(m, pid) {
		return true
	}
	if m.path(pid) != canonical(python) {
		return false
	}
	return syscall.Kill(pid, syscall.SIGKILL) == nil
}

func (x *experiment) deathExperiment(kind string) (o deathObservation) {
	o.Kind = kind
	c, e := x.task("sleep", true, x.exe)
	if e != nil {
		o.Error = e.Error()
		return
	}
	c.Budget.WallMS = 7000
	c.Receipt = filepath.Join(x.base, "receipt-"+kind+".json")
	c.AppReady = filepath.Join(x.base, "app-"+kind+".json")
	config := filepath.Join(x.base, "death-config-"+kind+".json")
	if e = writeJSON(config, c); e != nil {
		o.Error = e.Error()
		return
	}
	appCmd := exec.Command(x.exe, "__app", config)
	if e = appCmd.Start(); e != nil {
		o.Error = e.Error()
		return
	}
	o.AppPID = appCmd.Process.Pid
	defer func() {
		if !o.VictimWaited {
			_ = appCmd.Process.Kill()
			_ = appCmd.Wait()
		}
		if !o.TaskRootClean {
			x.cleanTasks(c.Root)
		}
	}()
	if !waitFile(x.ctx, c.AppReady, 3*time.Second) {
		o.Error = "app/guardian did not become ready"
		return
	}
	var ar appReceipt
	if e = readJSON(c.AppReady, &ar); e != nil || ar.AppPID != o.AppPID {
		o.Error = "invalid app receipt"
		return
	}
	o.GuardianPID = ar.GuardianPID
	if !waitFile(x.ctx, filepath.Join(c.Out, "ready.json"), 3*time.Second) {
		o.Error = "Python fixed payload did not enter"
		_ = syscall.Kill(o.GuardianPID, syscall.SIGKILL)
		return
	}
	var ready struct {
		PID  int    `json:"pid"`
		Mode string `json:"mode"`
	}
	if e = readJSON(filepath.Join(c.Out, "ready.json"), &ready); e != nil || ready.PID <= 0 {
		o.Error = "invalid Python marker"
		return
	}
	o.PythonPID = ready.PID
	o.PayloadEntered = x.monitor.path(o.PythonPID) == canonical(c.Python)
	if !o.PayloadEntered {
		o.Error = "Python executable identity mismatch"
		return
	}
	if kind == "app_death" {
		_ = appCmd.Process.Kill()
		_ = appCmd.Wait()
		o.VictimWaited = true
		if waitFile(x.ctx, c.Receipt, 2*time.Second) {
			var receipt deathReceipt
			if readJSON(c.Receipt, &receipt) == nil {
				o.Receipt = &receipt
				o.MechanismCleanup = receipt.MechanismCleanup && receipt.Execution.Entered && receipt.Execution.StopReason == "cancelled" && receipt.Execution.Usage.Waited
			}
		}
		o.PythonPresentBeforeRescue = !waitAbsent(x.ctx, x.monitor, o.PythonPID, 200*time.Millisecond)
	} else {
		if x.monitor.path(o.GuardianPID) != canonical(x.exe) {
			o.Error = "guardian executable identity mismatch"
			return
		}
		_ = syscall.Kill(o.GuardianPID, syscall.SIGKILL)
		_ = appCmd.Wait()
		o.VictimWaited = true
		// Observe the candidate deficiency before rescue. The fixed payload has
		// a five-second self bound; this observation is made after 100ms.
		time.Sleep(100 * time.Millisecond)
		o.PythonPresentBeforeRescue = !processAbsent(x.monitor, o.PythonPID)
		var receipt deathReceipt
		if readJSON(c.Receipt, &receipt) == nil {
			o.Receipt = &receipt
			o.MechanismCleanup = receipt.MechanismCleanup
		}
	}
	if o.PythonPresentBeforeRescue {
		o.HarnessRescue = true
		if !rescuePython(x.monitor, o.PythonPID, c.Python) {
			o.Error = "identity-checked rescue failed"
			return
		}
	}
	o.PythonAbsentAfterRescue = waitAbsent(x.ctx, x.monitor, o.PythonPID, 2*time.Second)
	if !o.PythonPresentBeforeRescue && kind == "app_death" {
		o.TaskRootClean = func() bool { _, e := os.Lstat(c.Root); return os.IsNotExist(e) }()
	} else {
		o.TaskRootClean = x.cleanTasks(c.Root)
	}
	return
}

func (x *experiment) lifecycleStage() {
	s := stageStart("lifecycle", []string{"Normal/error/cancel/timeout enter actual fixed payloads and Wait; application death triggers guardian EOF kill/Wait/cleanup; all experimental residue is cleaned."}, []string{"Guardian unexpected death: observe survival and mechanism cleanup before explicitly labelled harness rescue; rescue is not production capability."})
	roots := []string{}
	for _, tc := range []struct{ mode, check string }{{"vmem", "normal"}, {"error", "error"}, {"sleep", "cancel"}, {"wall", "timeout"}} {
		c, e := x.task(tc.mode, true, x.exe)
		if e != nil {
			s.check(tc.check, false, e.Error())
			continue
		}
		roots = append(roots, c.Root)
		ctx := x.ctx
		cancel := func() {}
		if tc.check == "cancel" {
			var stop context.CancelFunc
			ctx, stop = context.WithCancel(x.ctx)
			cancel = stop
			go func() {
				if waitFile(ctx, filepath.Join(c.Out, "ready.json"), 2*time.Second) {
					stop()
				}
			}()
		}
		if tc.check == "timeout" {
			c.Budget.WallMS = 200
		}
		run := runFixed(ctx, x.exe, c, x.monitor)
		cancel()
		s.Runs = append(s.Runs, run)
		pass := run.Entered && run.Usage.Waited
		switch tc.check {
		case "normal":
			pass = pass && goodRun(run)
		case "error":
			pass = pass && run.ExitCode != 0 && run.StopReason == ""
		case "cancel":
			pass = pass && run.StopReason == "cancelled" && run.ExitCode != 0
		case "timeout":
			pass = pass && run.StopReason == "wall_limit" && run.ExitCode != 0
		}
		s.check(tc.check, pass, "actual payload entry, exit/stop reason and Wait recorded")
	}
	appDeath := x.deathExperiment("app_death")
	guardianDeath := x.deathExperiment("guardian_death")
	s.check("app_death", appDeath.Error == "" && appDeath.PayloadEntered && appDeath.VictimWaited && appDeath.MechanismCleanup && !appDeath.HarnessRescue && appDeath.PythonAbsentAfterRescue && appDeath.TaskRootClean, "actual SIGKILL of app helper; mechanism receipt must prove EOF cancellation, child Wait and cleanup without harness rescue")
	s.check("guardian_death_observed", guardianDeath.Error == "" && guardianDeath.PayloadEntered && guardianDeath.VictimWaited && guardianDeath.PythonAbsentAfterRescue && guardianDeath.TaskRootClean, "actual guardian SIGKILL; record before-rescue survival rather than presume parent-death kill")
	s.check("rescue_separate", !guardianDeath.PythonPresentBeforeRescue || guardianDeath.HarnessRescue, "raw negative observation and explicit harness rescue are separate fields")
	s.check("waited", allWaited(s.Runs) && appDeath.VictimWaited && guardianDeath.VictimWaited, "direct children Waited; orphan absence observed separately, no false claim of harness wait4 on a grandchild")
	s.check("temp_clean", x.cleanTasks(roots...) && appDeath.TaskRootClean && guardianDeath.TaskRootClean, "normal and exceptional task roots absent")
	s.Details = map[string]interface{}{"app_death": appDeath, "guardian_death": guardianDeath}
	s.Limitations = []string{"Guardian death can orphan Python; the outer test harness rescues it. This candidate is not production-feasible until an independent lifecycle mechanism is validated."}
	x.report.stage(s)
}
