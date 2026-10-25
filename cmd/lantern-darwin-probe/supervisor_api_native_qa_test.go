//go:build darwin

package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDarwinSupervisorAPINativeIndependentQA(t *testing.T) {
	if os.Getenv("DARWIN_SUPERVISOR_MUTATION_QA") != "1" {
		t.Skip("requires explicit new supervisor API native mutation gate")
	}
	probe := qaNativeRequiredEnv(t, "DARWIN_SUPERVISOR_PROBE")
	baselinePath := qaNativeRequiredEnv(t, "DARWIN_SUPERVISOR_BASELINE_REPORT")
	repo := strings.TrimSpace(qaNativeCommand(t, "", "git", "rev-parse", "--show-toplevel"))
	head := strings.TrimSpace(qaNativeCommand(t, repo, "git", "rev-parse", "HEAD"))
	var baseline apiReport
	qaAPIReadJSON(t, baselinePath, &baseline)
	qaAPIAssertBaseline(t, baseline, head, repo, probe)
	out := filepath.Join(filepath.Dir(baselinePath), "independent-qa")
	if err := os.Mkdir(out, 0700); err != nil {
		t.Fatal("independent evidence must be new: ", err)
	}
	type record struct {
		Name           string `json:"name"`
		SourceFile     string `json:"source_file"`
		OriginalSHA    string `json:"original_sha256"`
		MutatedSHA     string `json:"mutated_sha256"`
		ReportPath     string `json:"report_path"`
		Experiment     string `json:"experiment"`
		ExitScope      string `json:"exit_scope"`
		Exit           int    `json:"exit_code"`
		ValidationExit int    `json:"validation_exit_code"`
		ObservedExit   int    `json:"observed_exit_code"`
		TargetRed      bool   `json:"target_red"`
		Restored       bool   `json:"source_restored"`
	}
	results := []record{}
	index := map[string]any{"candidate_sha": head, "baseline_status": baseline.Status, "baseline_report": baselinePath, "original_no_go_passed": false, "reuse_output_preserved": false}
	defer func() {
		index["status"] = "passed"
		if t.Failed() {
			index["status"] = "failed"
		}
		index["results"] = results
		if err := writeJSON(filepath.Join(out, "index.json"), index); err != nil {
			t.Error("independent mutation index was not persisted: ", err)
		}
	}()
	qaAPIRejectsReuse(t, probe, filepath.Join(out, "reuse-output"))
	index["reuse_output_preserved"] = true
	exit, control := qaAPIGOWitness(t, repo, probe, filepath.Join(out, "original-no-go"))
	if exit != 0 || control.Status != "passed" || !control.Prepared || !control.HeldWithoutGO || control.GOSent || control.Worker != nil || control.WorkerExit != 125 || !control.Waited || !control.FixtureCleaned || control.Source != head || control.Dirty {
		t.Fatalf("original new-core no-GO control did not pass: oracle=%d %+v", exit, control)
	}
	index["original_no_go_passed"] = true
	snapshot := t.TempDir()
	qaNativeArchive(t, repo, snapshot)
	qaNativeCommand(t, snapshot, "git", "init", "--quiet")
	qaNativeCommand(t, snapshot, "git", "add", "--all", "--force")
	qaNativeCommand(t, snapshot, "git", "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-c", "user.name=Lantern QA", "-c", "user.email=qa@localhost", "commit", "--quiet", "-m", "isolated new supervisor API QA source")
	snapshotSHA := strings.TrimSpace(qaNativeCommand(t, snapshot, "git", "rev-parse", "HEAD"))
	index["snapshot_commit"] = snapshotSHA
	for _, name := range []string{"host-watcher-disabled", "guardian-eof-disabled", "launcher-go-bypass"} {
		t.Run(name, func(t *testing.T) {
			rec := record{Name: name, SourceFile: "internal/supervisor/runtime_darwin.go"}
			path := filepath.Join(snapshot, rec.SourceFile)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			changed := qaAPIMutate(t, name, string(original))
			rec.OriginalSHA, rec.MutatedSHA = digest(original), digest([]byte(changed))
			if err := os.WriteFile(path, []byte(changed), 0600); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := os.WriteFile(path, original, 0600); err != nil {
					t.Error(err)
				}
				b, err := os.ReadFile(path)
				rec.Restored = err == nil && string(b) == string(original)
				if !rec.Restored {
					t.Error("isolated new-core source did not restore")
				}
				results = append(results, rec)
			}()
			dir := filepath.Join(out, name)
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			executable := filepath.Join(t.TempDir(), "probe")
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			cmd := exec.CommandContext(ctx, "go", "build", "-o", executable, "./cmd/lantern-darwin-probe")
			cmd.Dir, cmd.Env = snapshot, append(os.Environ(), "CGO_ENABLED=0")
			b, err := cmd.CombinedOutput()
			cancel()
			_ = os.WriteFile(filepath.Join(dir, "build.log"), b, 0600)
			if err != nil {
				t.Fatalf("compile failure cannot kill a lifecycle safeguard: %v %s", err, b)
			}
			if name == "launcher-go-bypass" {
				rec.Experiment, rec.ExitScope = "private-new-core-launcher-no-go-witness", "independent-go-test-assertion-process"
				rec.ValidationExit, control = qaAPIGOWitness(t, snapshot, executable, filepath.Join(dir, "witness"))
				rec.Exit, rec.ObservedExit = rec.ValidationExit, control.WorkerExit
				rec.ReportPath = filepath.Join(dir, "witness", "evidence.json")
				if rec.ValidationExit != 1 || control.Status != "failed" || !control.Prepared || control.GOSent || control.Worker == nil || !control.Worker.Entered || control.WorkerExit != 0 || !control.Waited || !control.FixtureCleaned || control.Source != snapshotSHA || !control.Dirty {
					t.Fatalf("new-core GO removal did not cause actual unauthorized entry: oracle=%d %+v", rec.ValidationExit, control)
				}
			} else {
				rec.Experiment, rec.ExitScope = "new-api-full-18-cases", "supervision-api-cli"
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
				defer cancel()
				cmd := exec.CommandContext(ctx, executable, "supervision-api", "--out", filepath.Join(dir, "run"))
				cmd.Dir = snapshot
				b, err := cmd.CombinedOutput()
				_ = os.WriteFile(filepath.Join(dir, "command.log"), b, 0600)
				if ctx.Err() != nil {
					t.Fatal("outer timeout is not new-core mutation evidence")
				}
				exit, ok := err.(*exec.ExitError)
				if !ok || exit.ExitCode() != 1 {
					t.Fatalf("real new-API oracle did not reject removed supervision: %v %s", err, b)
				}
				rec.Exit, rec.ObservedExit, rec.ValidationExit = 1, 1, 1
				rec.ReportPath = filepath.Join(dir, "run", "report.json")
				var r apiReport
				qaAPIReadJSON(t, rec.ReportPath, &r)
				if r.Status != "failed" || r.Source != snapshotSHA || !r.Dirty || len(r.Cases) != 18 || r.Error != "" {
					t.Fatal("mutant did not execute the complete new-API experiment set")
				}
				target := "steady-guardian"
				if name == "guardian-eof-disabled" {
					target = "steady-host"
				}
				var actual *apiCase
				for i := range r.Cases {
					if r.Cases[i].ID == target {
						actual = &r.Cases[i]
					}
				}
				if actual == nil || actual.Status != "failed" || !actual.HarnessRescue || actual.FaultAtNS <= 0 || actual.TaskPID <= 0 || actual.Checks["processes_absent"] || actual.WallMS >= 8000 {
					t.Fatalf("removed peer watcher did not leave a real registered task for labelled rescue: %+v", actual)
				}
				entered := false
				for _, e := range actual.Events {
					entered = entered || e.Kind == "payload_entered" && e.TargetPID == actual.TaskPID
				}
				if !entered || actual.Error != "fault response exceeded two seconds" {
					t.Fatalf("failure was not the actual post-entry fault-response promise: %+v", actual)
				}
			}
			rec.TargetRed = true
			t.Logf("actual new-core behavior killed %s: %s", name, rec.ReportPath)
		})
	}
}

func qaAPIReadJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

func qaAPIAssertBaseline(t *testing.T, r apiReport, head, repo, probe string) {
	t.Helper()
	if r.Schema != "lantern.darwin-supervisor-api.v1" || r.Status != "passed" || r.Source != head || r.Dirty || r.Error != "" || len(r.Cases) != 18 || r.Environment["goos"] != "darwin" || r.Environment["goarch"] != "arm64" {
		t.Fatal("complete actual new-API native baseline unavailable")
	}
	b, err := os.ReadFile(probe)
	if err != nil || digest(b) != r.ExecutableSHA {
		t.Fatal("baseline does not bind the actual new-API executable")
	}
	for _, file := range []string{"common.go", "protocol.go", "runtime_darwin.go", "spawn_darwin.go"} {
		b, err := os.ReadFile(filepath.Join(repo, "internal", "supervisor", file))
		if err != nil || digest(b) != r.Fingerprints[file] {
			t.Fatal("baseline source fingerprint mismatch: ", file)
		}
	}
	want := map[string]bool{"normal": true, "error": true, "cancel": true, "exit-cancel-race": true}
	for _, phase := range []string{"before-spawn", "after-spawn", "registered", "armed", "steady", "setsid", "setpgid"} {
		for _, target := range []string{"host", "guardian"} {
			prefix := ""
			if phase == "before-spawn" || phase == "after-spawn" || phase == "registered" || phase == "armed" {
				prefix = "startup-"
			}
			want[prefix+phase+"-"+target] = true
		}
	}
	for _, c := range r.Cases {
		if !want[c.ID] || c.Status != "passed" || c.Error != "" || c.HarnessRescue || len(c.Checks) < 11 || c.WallMS >= 8000 || !apiProtocolOrder(c) {
			t.Fatalf("native baseline case incomplete or duplicated: %+v", c)
		}
		delete(want, c.ID)
		for _, pass := range c.Checks {
			if !pass {
				t.Fatal("native baseline has a false required check: ", c.ID)
			}
		}
		if c.FaultTarget == "host" {
			if c.Return.Returned {
				t.Fatal("a killed actual caller was falsely said to return from Run")
			}
		} else if !c.Return.Returned || c.Return.HostPID != c.HostPID || c.Return.Result.HostPID != c.HostPID {
			t.Fatal("new API used a wrapper process instead of its actual caller")
		}
	}
	if len(want) != 0 {
		t.Fatal("native baseline required cases missing")
	}
}

func qaAPIMutate(t *testing.T, name, source string) string {
	t.Helper()
	switch name {
	case "host-watcher-disabled":
		old := "signalReceipt(m, &rr, &l)"
		if strings.Count(source, old) != 1 {
			t.Fatal("new Host watcher action must match once")
		}
		return strings.Replace(source, old, "_ = rr // independent QA removes actual Host peer-loss signal", 1)
	case "guardian-eof-disabled":
		old := "stop := func(reason string) {"
		start := strings.Index(source, "func guardianMain(")
		if start < 0 || strings.Count(source[start:], old) != 1 {
			t.Fatal("new guardian stop action must match once")
		}
		return source[:start] + strings.Replace(source[start:], old, old+"\nif reason == \"app_eof\" || reason == \"command_lost\" { return }", 1)
	case "launcher-go-bypass":
		start := strings.Index(source, "frames := messages(goR, c.Nonce)")
		if start < 0 {
			t.Fatal("new launcher GO wait missing")
		}
		end := strings.Index(source[start:], "\n\tselect {\n\tcase <-aGone:")
		if end < 0 {
			t.Fatal("new launcher post-GO peer observation boundary missing")
		}
		return source[:start] + "_ = goR // independent QA bypasses actual new-core GO wait\n" + source[start+end:]
	}
	t.Fatal("unknown new-API safeguard mutation")
	return source
}

func qaAPIGOWitness(t *testing.T, dir, probe, out string) (int, qaAPINoGO) {
	t.Helper()
	if err := os.MkdirAll(out, 0700); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestDarwinSupervisorAPINoGOWitness$", "-test.v")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "DARWIN_SUPERVISOR_GO_WITNESS=1", "DARWIN_SUPERVISOR_PROBE="+probe, "DARWIN_SUPERVISOR_WITNESS_OUT="+out)
	b, err := cmd.CombinedOutput()
	_ = os.WriteFile(filepath.Join(out, "witness.log"), b, 0600)
	if ctx.Err() != nil {
		t.Fatal("outer witness timeout is not proof of unauthorized GO")
	}
	code := 0
	if err != nil {
		e, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		code = e.ExitCode()
	}
	var r qaAPINoGO
	qaAPIReadJSON(t, filepath.Join(out, "evidence.json"), &r)
	return code, r
}

func qaAPIRejectsReuse(t *testing.T, probe, out string) {
	t.Helper()
	if err := os.Mkdir(out, 0700); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(out, "owned-old-pid-ready")
	child := exec.Command(probe, "__signal-target", ready)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	childWaited := false
	defer func() {
		if !childWaited {
			_ = child.Process.Kill()
			<-done
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if !waitFile(ctx, ready, time.Second) {
		t.Fatal("owned stale-PID positive control not ready")
	}
	old, err := json.Marshal(map[string]any{"prior": "untouched", "host_pid": child.Process.Pid})
	if err != nil {
		t.Fatal(err)
	}
	prior := filepath.Join(out, "report.json")
	if err := os.WriteFile(prior, old, 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(out, "old-event.json"), map[string]any{"actor": "host", "pid": child.Process.Pid}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, probe, "supervision-api", "--out", out)
	b, err := cmd.CombinedOutput()
	e, ok := err.(*exec.ExitError)
	if ctx.Err() != nil || !ok || e.ExitCode() != 1 {
		t.Fatalf("new CLI reuse failed for the wrong reason: %v %s", err, b)
	}
	var r apiReport
	if json.Unmarshal(b, &r) != nil || r.Status != "failed" || len(r.Cases) != 0 {
		t.Fatal("reuse was not rejected before actual actors started")
	}
	if got, err := os.ReadFile(prior); err != nil || string(got) != string(old) {
		t.Fatal("new CLI overwrote old output")
	}
	select {
	case <-done:
		childWaited = true
		t.Fatal("new CLI signalled an owned stale PID")
	default:
	}
	if err := writeJSON(filepath.Join(out, "reuse-evidence.json"), map[string]any{"status": "passed", "prior_output_unchanged": true, "old_pid_not_signalled": true, "new_cases": 0}); err != nil {
		t.Fatal(err)
	}
}
