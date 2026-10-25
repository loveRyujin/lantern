//go:build darwin

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/internal/supervisor"
)

type qaAPINoGO struct {
	Schema         string                   `json:"schema"`
	Status         string                   `json:"status"`
	Prepared       bool                     `json:"prepared"`
	HeldWithoutGO  bool                     `json:"held_without_go"`
	GOSent         bool                     `json:"go_sent"`
	Worker         *supervisor.WorkerResult `json:"worker,omitempty"`
	WorkerExit     int                      `json:"worker_exit_code"`
	Waited         bool                     `json:"waited"`
	FixtureCleaned bool                     `json:"fixture_cleaned_by_observer"`
	Source         string                   `json:"source_commit"`
	Dirty          bool                     `json:"source_dirty"`
}

func TestDarwinSupervisorAPINoGOWitness(t *testing.T) {
	if os.Getenv("DARWIN_SUPERVISOR_GO_WITNESS") != "1" {
		t.Skip("private native no-GO validation subprocess")
	}
	probe := qaNativeRequiredEnv(t, "DARWIN_SUPERVISOR_PROBE")
	out := qaNativeRequiredEnv(t, "DARWIN_SUPERVISOR_WITNESS_OUT")
	if err := os.MkdirAll(out, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	r := qaAPINoGO{Schema: "lantern.supervisor-api-no-go-qa.v1", Status: "invalid", WorkerExit: -999}
	work, err := os.MkdirTemp(out, "owned-work-")
	if err != nil {
		t.Fatal(err)
	}
	evidence := filepath.Join(out, "launcher-evidence")
	if err := os.Mkdir(evidence, 0700); err != nil {
		t.Fatal(err)
	}
	nonce := strings.Repeat("a", 32)
	config := filepath.Join(out, "job.json")
	if err := writeJSON(config, map[string]any{"nonce": nonce, "request": supervisor.Request{Kind: supervisor.GoLifecycle, Mode: supervisor.Normal}, "work": work, "evidence": evidence, "retain_evidence": true, "control_fds": []supervisor.FDIdentity{}}); err != nil {
		t.Fatal(err)
	}
	pipe := func() (*os.File, *os.File) {
		a, b, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { a.Close(); b.Close() })
		return a, b
	}
	regR, regW := pipe()
	goR, goW := pipe()
	guardianR, guardianW := pipe()
	hostR, hostW := pipe()
	permitR, permitW := pipe()
	outR, outW := pipe()
	// The observer retains both peer-life writers and never sends any GO byte.
	// Keeping peers alive prevents EOF watchers from hiding a removed GO gate.
	_ = guardianW
	_ = hostW
	cmd := exec.Command(probe, "--lantern-internal-supervisor-v1", "launcher", config)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C", "GOMAXPROCS=2"}
	cmd.ExtraFiles = []*os.File{regW, goR, guardianR, hostR, permitR}
	cmd.Stdout = outW
	stderr := &boundedBuffer{limit: 64 << 10, overflow: make(chan struct{})}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []*os.File{regW, goR, guardianR, hostR, permitR, outW} {
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
		_ = cleanupFixtureRoot(work)
	}()
	if _, err := permitW.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	permitW.Close()
	type wire struct {
		Version int                 `json:"version"`
		Kind    string              `json:"kind"`
		Seq     int                 `json:"seq"`
		Nonce   string              `json:"nonce"`
		Target  supervisor.Identity `json:"target"`
	}
	frames := make(chan wire, 2)
	go func() {
		defer close(frames)
		s := bufio.NewScanner(regR)
		s.Buffer(make([]byte, 512), 4097)
		for s.Scan() {
			var v wire
			if json.Unmarshal(s.Bytes(), &v) != nil {
				return
			}
			frames <- v
		}
	}()
	for i, kind := range []string{"BOOTSTRAP", "PREPARED"} {
		select {
		case v, ok := <-frames:
			if !ok || v.Version != 1 || v.Kind != kind || v.Seq != i+1 || v.Nonce != nonce || v.Target.PID != cmd.Process.Pid || v.Target.Nonce != nonce || v.Target.StartSeconds == 0 || v.Target.Image != canonical(probe) {
				t.Fatalf("no valid direct new-core registration: %+v", v)
			}
		case <-ctx.Done():
			t.Fatal("no direct registration; deadline is not GO-gate mutation evidence")
		}
	}
	r.Prepared = true
	markers := make(chan *supervisor.WorkerResult, 1)
	go func() {
		var v supervisor.WorkerResult
		s := bufio.NewScanner(outR)
		s.Buffer(make([]byte, 512), 4097)
		if s.Scan() && json.Unmarshal(s.Bytes(), &v) == nil {
			markers <- &v
		} else {
			markers <- nil
		}
	}()
	hold := time.NewTimer(300 * time.Millisecond)
	defer hold.Stop()
	finished := time.NewTimer(2 * time.Second)
	defer finished.Stop()
	for !waited {
		select {
		case marker := <-markers:
			markers = nil
			r.Worker = marker
		case <-hold.C:
			if r.Worker == nil {
				r.HeldWithoutGO = true
				goW.Close() // Deliberate abort: no GO has ever been written.
			}
		case <-done:
			waited = true
		case <-finished.C:
			t.Fatal("owned no-GO fixture did not terminate; generic timeout is not evidence")
		case <-ctx.Done():
			t.Fatal("outer fixture deadline is not evidence")
		}
	}
	if markers != nil {
		select {
		case r.Worker = <-markers:
		case <-ctx.Done():
			t.Fatal("worker marker stream did not drain after owned Wait")
		}
	}
	r.Waited, r.WorkerExit = true, cmd.ProcessState.ExitCode()
	if r.Worker != nil && (!r.Worker.Entered || r.Worker.PID != cmd.Process.Pid || r.Worker.Mode != supervisor.Normal) {
		t.Fatal("output was not the actual fixed new-core Go helper marker")
	}
	if err := cleanupFixtureRoot(work); err != nil {
		t.Fatal(err)
	}
	r.FixtureCleaned = true // This standalone fixture explicitly owns cleanup.
	r.Source = commandText(ctx, "git", "rev-parse", "HEAD")
	r.Dirty = commandText(ctx, "git", "status", "--porcelain") != ""
	r.Status = "passed"
	if r.Worker != nil {
		r.Status = "failed"
	}
	if err := writeJSON(filepath.Join(out, "evidence.json"), r); err != nil {
		t.Fatal(err)
	}
	if r.Worker != nil {
		t.Fatal("actual new-core Go helper entered without any GO authorization")
	}
	if !r.HeldWithoutGO || r.WorkerExit != 125 {
		t.Fatalf("invalid original no-GO control: %+v", r)
	}
}
