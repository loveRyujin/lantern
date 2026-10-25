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

	"github.com/loveRyujin/lantern/core/pyrt"
)

type qaSupervisionGoEvidence struct {
	Schema         string              `json:"schema"`
	Status         string              `json:"status"`
	Prepared       bool                `json:"prepared"`
	GoSent         bool                `json:"go_sent"`
	HeldWithoutGo  bool                `json:"held_without_go"`
	PayloadEntered bool                `json:"payload_entered"`
	Payload        *supervisionMarker  `json:"payload,omitempty"`
	Identity       supervisionIdentity `json:"identity"`
	WorkerExit     int                 `json:"worker_exit_code"`
	Waited         bool                `json:"waited"`
	BreachCleanup  bool                `json:"observer_cleanup_after_breach"`
	TaskClean      bool                `json:"task_clean"`
	Source         string              `json:"source_commit"`
	SourceDirty    string              `json:"source_dirty"`
}

// Invoked only in an owned test subprocess. Its real exit status is the oracle
// status in the mutation index; the worker's different exit is kept separately.
func TestDarwinSupervisionNoGOWitness(t *testing.T) {
	if os.Getenv("DARWIN_SUPERVISION_GO_WITNESS") != "1" {
		t.Skip("private native no-GO witness")
	}
	probe := qaNativeRequiredEnv(t, "DARWIN_SUPERVISION_WITNESS_PROBE")
	config := qaNativeRequiredEnv(t, "DARWIN_SUPERVISION_RUNTIME_CONFIG")
	out := qaNativeRequiredEnv(t, "DARWIN_SUPERVISION_WITNESS_OUT")
	r := qaSupervisionWithoutGo(t, probe, config, out)
	if err := writeJSON(filepath.Join(out, "evidence.json"), r); err != nil {
		t.Fatal(err)
	}
	if r.PayloadEntered {
		t.Fatal("actual payload entered while the private GO writer sent no authorization")
	}
	if !r.Prepared || !r.HeldWithoutGo || !r.Waited || r.WorkerExit != 125 || !r.TaskClean || r.BreachCleanup {
		t.Fatalf("invalid original no-GO control: %+v", r)
	}
}

func qaSupervisionWithoutGo(t *testing.T, probe, config, out string) (r qaSupervisionGoEvidence) {
	t.Helper()
	r = qaSupervisionGoEvidence{Schema: "lantern.darwin-supervision-no-go-qa.v1", Status: "invalid", WorkerExit: -999}
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	rt, err := readRuntime(config)
	if err != nil {
		t.Fatal(err)
	}
	release, err := pyrt.AcquirePrepared(ctx, rt)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	plan, err := runtimeReadPlan(rt)
	if err != nil {
		t.Fatal(err)
	}
	m, err := newProcessMonitor()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(out, "policies"), 0700); err != nil {
		t.Fatal(err)
	}
	base := canonical(t.TempDir())
	x := &experiment{ctx: ctx, out: out, base: base, exe: probe, rt: rt, plan: plan, monitor: m}
	task, err := x.task("sleep", true, probe)
	if err != nil {
		t.Fatal(err)
	}
	task.Budget.WallMS = 5000
	control := filepath.Join(out, "control")
	if err := createSupervisionControl(control); err != nil {
		t.Fatal(err)
	}
	c := supervisionConfig{ID: "normal", Nonce: strings.Repeat("a", 32), Window: "normal", Mode: "normal", Control: control, Exe: probe, Task: task}
	if err := os.WriteFile(task.Script, supervisionPayload, 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(control, "config.json")
	if err := writeJSON(path, c); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(task.Root, "input", "supervision-task.json"), c); err != nil {
		t.Fatal(err)
	}
	pipe := func() (*os.File, *os.File) {
		a, b, e := os.Pipe()
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { a.Close(); b.Close() })
		return a, b
	}
	regR, regW := pipe()
	goR, goW := pipe()
	guardR, guardW := pipe()
	appR, appW := pipe()
	permitR, permitW := pipe()
	// Only this observer owns the GO and both liveness writers. No actor is
	// killed during the observation, so another watcher cannot hide the breach.
	_ = guardW
	_ = appW
	cmd := exec.Command(probe, "__supervision-launcher", path)
	cmd.Env = fixedEnv(task)
	cmd.ExtraFiles = []*os.File{regW, goR, guardR, appR, permitR}
	stdout := &boundedBuffer{limit: 64 << 10, overflow: make(chan struct{})}
	stderr := &boundedBuffer{limit: 64 << 10, overflow: make(chan struct{})}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []*os.File{regW, goR, guardR, appR, permitR} {
		f.Close()
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Process.Kill()
			<-done
		}
		_ = cleanupFixtureRoot(task.Root)
	}()
	if _, err := permitW.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	permitW.Close()
	frames := supervisionFrames(regR, c.Nonce)
	select {
	case f := <-frames:
		if f.Err != nil || f.Frame.Kind != "PREPARED" || f.Frame.PID != cmd.Process.Pid {
			t.Fatalf("no real direct registration: %+v", f)
		}
		r.Identity = supervisionIdentityFrom(f.Frame)
		r.Prepared = true
		if r.Identity.Image != canonical(probe) {
			t.Fatal("prepared image was not the owned fixed launcher")
		}
	case <-done:
		waited = true
		t.Fatal("launcher exited before PREPARED")
	case <-time.After(2 * time.Second):
		t.Fatal("direct PREPARED registration was not observed")
	}
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	hold := time.NewTimer(300 * time.Millisecond)
	defer hold.Stop()
	observeUntil := time.NewTimer(2 * time.Second)
	defer observeUntil.Stop()
	goClosed := false
	for !waited {
		var marker supervisionMarker
		if readJSON(filepath.Join(task.Out, "marker.json"), &marker) == nil && marker.PID == cmd.Process.Pid {
			r.PayloadEntered = true
			r.Payload = &marker
			// Preserve the fixed normal payload's actual natural exit status.
			// The separate witness process fails its no-GO assertion afterward.
		}
		select {
		case <-done:
			waited = true
		case <-hold.C:
			if !r.PayloadEntered {
				r.HeldWithoutGo = true
				goW.Close()
				goClosed = true
			}
		case <-tick.C:
		case <-observeUntil.C:
			t.Fatal("no-GO witness could not finish its explicit EOF/owned-child cleanup")
		case <-ctx.Done():
			t.Fatal("outer deadline cannot be no-GO evidence")
		}
	}
	// Wait can win the select just after the fixed normal payload writes its
	// marker. Read the retained file after Wait as well; no polling interval is
	// used as proof that unauthorized execution did not happen.
	var finalMarker supervisionMarker
	if readJSON(filepath.Join(task.Out, "marker.json"), &finalMarker) == nil && finalMarker.PID == cmd.Process.Pid {
		r.PayloadEntered = true
		r.Payload = &finalMarker
	}
	if !goClosed && !r.PayloadEntered {
		t.Fatal("launcher exited before the deliberate GO EOF")
	}
	r.Waited = true
	r.WorkerExit = cmd.ProcessState.ExitCode()
	if err := cleanupFixtureRoot(task.Root); err != nil {
		t.Fatal(err)
	}
	r.TaskClean = true
	r.Source = commandText(ctx, "git", "rev-parse", "HEAD")
	r.SourceDirty = commandText(ctx, "git", "status", "--porcelain")
	if r.SourceDirty != "" {
		r.SourceDirty = "true"
	} else {
		r.SourceDirty = "false"
	}
	r.Status = "passed"
	if r.PayloadEntered {
		r.Status = "failed"
	}
	return r
}

func qaSupervisionReadGoEvidence(t *testing.T, path string) qaSupervisionGoEvidence {
	t.Helper()
	b, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	var r qaSupervisionGoEvidence
	if e = json.Unmarshal(b, &r); e != nil {
		t.Fatal(e)
	}
	return r
}
