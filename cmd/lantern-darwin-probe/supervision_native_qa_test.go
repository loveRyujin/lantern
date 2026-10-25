//go:build darwin

package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDarwinSupervisionNativeIndependentQA(t *testing.T) {
	if os.Getenv("DARWIN_SUPERVISION_REAL_QA") != "1" {
		t.Skip("requires explicit native supervision QA gate")
	}
	probe := qaNativeRequiredEnv(t, "DARWIN_SUPERVISION_PROBE")
	runtimeConfig := qaNativeRequiredEnv(t, "DARWIN_SUPERVISION_RUNTIME_CONFIG")
	baselinePath := qaNativeRequiredEnv(t, "DARWIN_SUPERVISION_BASELINE_REPORT")
	repo := strings.TrimSpace(qaNativeCommand(t, "", "git", "rev-parse", "--show-toplevel"))
	head := strings.TrimSpace(qaNativeCommand(t, repo, "git", "rev-parse", "HEAD"))
	r := qaReadSupervisionReport(t, baselinePath)
	qaAssertSupervisionBaseline(t, r, head)
	b, err := os.ReadFile(probe)
	if err != nil || digest(b) != r.Fingerprints["executable_sha256"] {
		t.Fatalf("baseline actual executable mismatch: %v", err)
	}
	rt, err := readRuntime(runtimeConfig)
	if err != nil || !reflect.DeepEqual(rt, r.Runtime) {
		t.Fatalf("baseline actual prepared runtime mismatch: %v", err)
	}
	evidence := filepath.Join(filepath.Dir(baselinePath), "independent-qa")
	if err := os.Mkdir(evidence, 0700); err != nil {
		t.Fatal("native QA evidence directory must be new: ", err)
	}
	qaSupervisionRejectsReusedOutput(t, probe, runtimeConfig, filepath.Join(evidence, "reuse-output"))
	controlCode, control := qaRunSupervisionGoWitness(t, repo, probe, runtimeConfig, filepath.Join(evidence, "original-no-go"))
	if controlCode != 0 || control.Status != "passed" || control.PayloadEntered || control.GoSent || !control.HeldWithoutGo || !control.TaskClean {
		t.Fatalf("original launcher no-GO control invalid: exit=%d evidence=%+v", controlCode, control)
	}
	snapshot := t.TempDir()
	qaNativeArchive(t, repo, snapshot)
	qaNativeCommand(t, snapshot, "git", "init", "--quiet")
	qaNativeCommand(t, snapshot, "git", "add", "--all", "--force")
	qaNativeCommand(t, snapshot, "git", "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-c", "user.name=Lantern QA", "-c", "user.email=qa@localhost", "commit", "--quiet", "-m", "isolated supervision QA source snapshot")
	snapshotSHA := strings.TrimSpace(qaNativeCommand(t, snapshot, "git", "rev-parse", "HEAD"))
	type result struct {
		Name           string `json:"name"`
		SourceFile     string `json:"source_file"`
		OriginalSHA    string `json:"original_sha256"`
		MutatedSHA     string `json:"mutated_sha256"`
		ReportPath     string `json:"report_path"`
		Experiment     string `json:"experiment"`
		Exit           int    `json:"exit_code"`
		ExitScope      string `json:"exit_scope"`
		ValidationExit int    `json:"validation_exit_code"`
		ObservedExit   int    `json:"observed_exit_code"`
		TargetRed      bool   `json:"target_red"`
		Restored       bool   `json:"source_restored"`
	}
	results := []result{}
	defer func() {
		if e := writeJSON(filepath.Join(evidence, "index.json"), map[string]interface{}{"candidate_sha": head, "snapshot_commit": snapshotSHA, "baseline_report": baselinePath, "baseline_status": r.Status, "platform": "actual native Darwin", "reuse_output_preserved": true, "original_no_go_passed": true, "results": results}); e != nil {
			t.Error("native QA index was not persisted: ", e)
		}
	}()
	for _, name := range []string{"app-watcher-disabled", "guardian-eof-disabled", "launcher-go-bypass"} {
		t.Run(name, func(t *testing.T) {
			file := "supervision_runtime_darwin.go"
			path := filepath.Join(snapshot, "cmd", "lantern-darwin-probe", file)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			changed := qaSupervisionMutate(t, name, string(original))
			rec := result{Name: name, SourceFile: file, OriginalSHA: digest(original), MutatedSHA: digest([]byte(changed))}
			if err := os.WriteFile(path, []byte(changed), 0600); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if e := os.WriteFile(path, original, 0600); e != nil {
					t.Error(e)
				}
				v, e := os.ReadFile(path)
				rec.Restored = e == nil && string(v) == string(original)
				if !rec.Restored {
					t.Error("mutation source not restored")
				}
				results = append(results, rec)
			}()
			out := filepath.Join(evidence, name)
			if err := os.Mkdir(out, 0700); err != nil {
				t.Fatal(err)
			}
			executable := filepath.Join(t.TempDir(), "probe")
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			build := exec.CommandContext(ctx, "go", "build", "-o", executable, "./cmd/lantern-darwin-probe")
			build.Dir = snapshot
			build.Env = append(os.Environ(), "CGO_ENABLED=0")
			b, e := build.CombinedOutput()
			cancel()
			_ = os.WriteFile(filepath.Join(out, "build.log"), b, 0600)
			if e != nil {
				t.Fatalf("compile failure is not a killed safeguard: %v %s", e, b)
			}
			if name == "launcher-go-bypass" {
				rec.Experiment = "private-launcher-no-go-witness"
				rec.ExitScope = "independent-go-test-assertion-process"
				rec.Exit, control = qaRunSupervisionGoWitness(t, snapshot, executable, runtimeConfig, filepath.Join(out, "witness"))
				rec.ValidationExit = rec.Exit
				rec.ObservedExit = control.WorkerExit
				rec.ReportPath = filepath.Join(out, "witness", "evidence.json")
				if rec.Exit != 1 || control.Status != "failed" || !control.Prepared || control.GoSent || !control.PayloadEntered || !control.Waited || !control.TaskClean || control.WorkerExit != 0 || control.BreachCleanup || control.Payload == nil {
					t.Fatalf("GO removal did not produce actual unauthorized entry: exit=%d evidence=%+v", rec.Exit, control)
				}
				if control.Source != snapshotSHA || control.SourceDirty != "true" {
					t.Fatal("GO witness source provenance mismatch")
				}
			} else {
				rec.Experiment = "full-supervision-fixed-18-cases"
				rec.ExitScope = "supervision-subcommand"
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
				defer cancel()
				cmd := exec.CommandContext(ctx, executable, "supervision", "--runtime-config", runtimeConfig, "--out", filepath.Join(out, "run"))
				cmd.Dir = snapshot
				b, e := cmd.CombinedOutput()
				_ = os.WriteFile(filepath.Join(out, "command.log"), b, 0600)
				if ctx.Err() != nil {
					t.Fatal("outer timeout is not mutation evidence")
				}
				exit, ok := e.(*exec.ExitError)
				if !ok {
					t.Fatalf("removed action must fail its real oracle, error=%v output=%s", e, b)
				}
				rec.Exit = exit.ExitCode()
				rec.ObservedExit = rec.Exit
				rec.ReportPath = filepath.Join(out, "run", "report.json")
				mutant := qaReadSupervisionReport(t, rec.ReportPath)
				if rec.Exit != 1 || mutant.Status != "failed" || mutant.SingleFaultVerified || len(mutant.Cases) != 18 {
					t.Fatalf("mutant did not reach all actual experiments: exit=%d status=%s cases=%d", rec.Exit, mutant.Status, len(mutant.Cases))
				}
				if mutant.Environment["source_commit"] != snapshotSHA || mutant.Environment["source_dirty"] != "true" {
					t.Fatal("mutant source provenance mismatch")
				}
				if len(mutant.Preflight) != 5 {
					t.Fatal("mutant preflight incomplete")
				}
				for _, c := range mutant.Preflight {
					if !c.Passed {
						t.Fatal("preflight failure cannot kill this safeguard")
					}
				}
				caseID := "steady-guardian"
				if name == "guardian-eof-disabled" {
					caseID = "steady-app"
				}
				var actual *SupervisionCase
				for i := range mutant.Cases {
					if mutant.Cases[i].ID == caseID {
						actual = &mutant.Cases[i]
					}
				}
				if actual == nil || actual.Status != "failed" || !actual.Registered || !actual.Armed || !actual.GoAuthorized || !actual.PayloadEntered || actual.FaultAtNS <= 0 || actual.PayloadAbsent || !actual.HarnessRescue {
					t.Fatalf("removed peer-loss action did not leave the entered task alive until labelled rescue: %+v", actual)
				}
				if strings.Contains(actual.Error, "outer context") || strings.Contains(actual.Error, "exceeded 8s") {
					t.Fatal("generic case timeout is not the required post-fault liveness failure")
				}
			}
			rec.TargetRed = true
			t.Logf("actual behavior killed %s; experiment=%s evidence=%s", name, rec.Experiment, rec.ReportPath)
		})
	}
}

func qaReadSupervisionReport(t *testing.T, path string) SupervisionReport {
	t.Helper()
	b, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	var r SupervisionReport
	if e = json.Unmarshal(b, &r); e != nil {
		t.Fatal(e)
	}
	return r
}

func qaAssertSupervisionBaseline(t *testing.T, r SupervisionReport, head string) {
	t.Helper()
	if r.Schema != "lantern.darwin-supervision.v1" || r.Status != "passed" || !r.SingleFaultVerified || r.Production || !r.Cleanup || r.Error != "" || len(r.Cases) != 18 {
		t.Fatal("complete real supervision baseline unavailable")
	}
	if r.Environment["os"] != "darwin" || r.Environment["arch"] != "arm64" || r.Environment["source_commit"] != head || r.Environment["source_dirty"] != "false" {
		t.Fatal("native baseline provenance mismatch")
	}
	if r.Fingerprints["protocol_sha256"] != digest([]byte(supervisionProtocol)) || r.Fingerprints["payload_sha256"] != digest(supervisionPayload) {
		t.Fatal("baseline protocol/payload differs from actual source")
	}
	for _, key := range qaSupervisionHashes {
		if len(r.Fingerprints[key]) != 64 {
			t.Fatal("missing fingerprint ", key)
		}
	}
	if len(r.Preflight) != len(qaSupervisionPreflight) {
		t.Fatal("exact preflight manifest missing")
	}
	for _, name := range qaSupervisionPreflight {
		found := 0
		for _, v := range r.Preflight {
			if v.Name == name && v.Passed {
				found++
			}
		}
		if found != 1 {
			t.Fatal("preflight evidence missing ", name)
		}
	}
	seen := map[string]bool{}
	for _, c := range r.Cases {
		if seen[c.ID] || c.Status != "passed" || c.Error != "" || c.HarnessRescue || !c.AppAbsent || !c.GuardianAbsent || !c.PayloadAbsent || !c.TaskClean || !c.MechanismCleanup {
			t.Fatalf("native case incomplete: %+v", c)
		}
		seen[c.ID] = true
		if e := validateSupervisionCase(c); e != nil {
			t.Fatalf("invalid raw case %s: %v", c.ID, e)
		}
		if len(c.Checks) != 8 {
			t.Fatal("incomplete checks ", c.ID)
		}
		for _, name := range qaSupervisionChecks {
			found := 0
			for _, v := range c.Checks {
				if v.Name == name && v.Passed {
					found++
				}
			}
			if found != 1 {
				t.Fatal("missing real assertion ", c.ID, "/", name)
			}
		}
	}
	for _, id := range qaSupervisionIDs {
		if !seen[id] {
			t.Fatal("missing real case ", id)
		}
	}
}

func qaSupervisionMutate(t *testing.T, name, source string) string {
	t.Helper()
	changed := source
	switch name {
	case "app-watcher-disabled":
		old := "supervisionSignalResult(&r, &log, supervisionKill(m, r.Target))"
		start := strings.Index(source, "func supervisionApp(")
		if start < 0 || strings.Count(source[start:], old) != 1 {
			t.Fatal("App kill action must match exactly once")
		}
		changed = source[:start] + strings.Replace(source[start:], old, "supervisionSignalResult(&r, &log, nil)", 1)
	case "guardian-eof-disabled":
		old := "stop := func(reason string) {"
		if strings.Count(source, old) != 1 {
			t.Fatal("guardian termination policy must match exactly once")
		}
		// Both liveness EOF and command EOF notify the same App loss. Removing
		// only one reader is redundant; remove their common termination action.
		changed = strings.Replace(source, old, old+"\nif reason == \"app_eof\" || reason == \"command_lost\" { return }", 1)
	case "launcher-go-bypass":
		start := strings.Index(source, "frames := supervisionFrames(goRead, c.Nonce)")
		if start < 0 {
			t.Fatal("actual launcher GO reader missing")
		}
		end := strings.Index(source[start:], "// A previously observed loss")
		if end < 0 {
			t.Fatal("actual launcher GO wait boundary missing")
		}
		end += start
		changed = source[:start] + "_ = goRead // independent QA bypass of actual GO wait\n\t" + source[end:]
	default:
		t.Fatal("unknown controlled mutation")
	}
	if changed == source {
		t.Fatal("mutation did not change actual source")
	}
	return changed
}

func qaRunSupervisionGoWitness(t *testing.T, dir, probe, config, out string) (int, qaSupervisionGoEvidence) {
	t.Helper()
	if err := os.MkdirAll(out, 0700); err != nil {
		t.Fatal(err)
	}
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestDarwinSupervisionNoGOWitness$", "-test.v")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "DARWIN_SUPERVISION_GO_WITNESS=1", "DARWIN_SUPERVISION_WITNESS_PROBE="+probe, "DARWIN_SUPERVISION_RUNTIME_CONFIG="+config, "DARWIN_SUPERVISION_WITNESS_OUT="+out)
	b, e := cmd.CombinedOutput()
	_ = os.WriteFile(filepath.Join(out, "witness.log"), b, 0600)
	if ctx.Err() != nil {
		t.Fatal("outer witness timeout is not no-GO evidence")
	}
	code := 0
	if e != nil {
		exit, ok := e.(*exec.ExitError)
		if !ok {
			t.Fatal(e)
		}
		code = exit.ExitCode()
	}
	return code, qaSupervisionReadGoEvidence(t, filepath.Join(out, "evidence.json"))
}

func qaSupervisionRejectsReusedOutput(t *testing.T, probe, config, out string) {
	t.Helper()
	events := filepath.Join(out, "events", "steady-guardian")
	if e := os.MkdirAll(events, 0700); e != nil {
		t.Fatal(e)
	}
	ready := filepath.Join(out, "owned-signal-target")
	child := exec.Command(probe, "__signal-target", ready)
	if e := child.Start(); e != nil {
		t.Fatal(e)
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
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	if !waitFile(ctx, ready, time.Second) {
		t.Fatal("owned prior-PID control not ready")
	}
	oldEvent, _ := json.Marshal(SupervisionEvent{Actor: "app", Seq: 1, AtNS: time.Now().UnixNano(), Kind: "guardian_started", PID: child.Process.Pid, TargetPID: child.Process.Pid})
	files := map[string][]byte{filepath.Join(out, "report.json"): []byte(`{"prior":"preserve"}`), filepath.Join(out, "report.md"): []byte("prior markdown"), filepath.Join(events, "app.jsonl"): append(oldEvent, '\n')}
	for p, b := range files {
		if e := os.WriteFile(p, b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	cmd := exec.CommandContext(ctx, probe, "supervision", "--runtime-config", config, "--out", out)
	b, e := cmd.CombinedOutput()
	exit, ok := e.(*exec.ExitError)
	if ctx.Err() != nil || !ok || exit.ExitCode() != 1 {
		t.Fatalf("reuse rejection did not exit1: %v %s", e, b)
	}
	var r SupervisionReport
	if e = json.Unmarshal(b, &r); e != nil || r.Status != "failed" || !r.PreserveExistingOutput || len(r.Cases) != 0 || r.SingleFaultVerified {
		t.Fatalf("reuse read old events or started actors: %+v %v", r, e)
	}
	for p, want := range files {
		got, e := os.ReadFile(p)
		if e != nil || string(got) != string(want) {
			t.Fatal("reuse modified prior data: ", p)
		}
	}
	select {
	case <-done:
		childWaited = true
		t.Fatal("reuse signalled the owned old PID")
	default:
	}
	data, e := os.ReadFile(ready)
	if e != nil || string(data) != "ready" {
		t.Fatal("reuse changed the owned signal-target state")
	}
	_ = writeJSON(filepath.Join(out, "reuse-evidence.json"), map[string]interface{}{"status": "passed", "prior_files_unchanged": true, "old_pid_not_signalled": true, "new_cases": len(r.Cases)})
}
