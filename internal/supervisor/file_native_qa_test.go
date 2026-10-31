//go:build darwin

package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func qaFileNativeProof(t *testing.T) *qaSupervisorProof {
	t.Helper()
	if os.Getenv("DARWIN_FILE_REAL_QA") != "1" {
		t.Skip("requires explicit native file-budget acceptance gate")
	}
	out := os.Getenv("DARWIN_FILE_QA_OUT")
	if !filepath.IsAbs(out) || filepath.Clean(out) != out {
		t.Fatal("DARWIN_FILE_QA_OUT must be absolute and clean")
	}
	if err := os.MkdirAll(out, 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(out, t.Name()+".json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		b, err := exec.Command("git", args...).Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(b))
	}
	p := &qaSupervisorProof{Name: t.Name(), Source: git("rev-parse", "HEAD"), Dirty: git("status", "--porcelain") != "", HostPID: os.Getpid(), Observations: map[string]any{}}
	if head := strings.TrimSpace(os.Getenv("LANTERN_PR_HEAD")); len(head) == 40 {
		p.Observations["pr_head"] = head
	}
	t.Cleanup(func() {
		p.Status = "passed"
		if t.Failed() {
			p.Status = "failed"
		}
		if err := json.NewEncoder(f).Encode(p); err != nil {
			t.Error(err)
		}
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})
	return p
}

func fileNativeBudget(bytes, entries uint64, wall time.Duration) ResourceBudget {
	return ResourceBudget{CPU: 3 * time.Second, RSSBytes: 256 << 20, Wall: wall, FileBytes: bytes, FileEntries: entries}
}

func TestFileQANativeRecoveryAndCancellation(t *testing.T) {
	p := qaFileNativeProof(t)
	limits, environment := qaSupervisorLimits(t), os.Environ()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	s, cfg := qaSupervisorNative(t)
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	q := Request{Kind: GoResource, Mode: FileGrow, Budget: fileNativeBudget(48<<10, 0, 3*time.Second)}
	r, err := s.Run(ctx, q)
	qaSupervisorRecord(p, q, r, err)
	qaResourceTerminal(t, cfg, r)
	if !errors.Is(err, ErrFileLimit) || r.State != Failed || r.StopReason != "file_limit" || r.Usage.FileTriggerAxis != "allocated_bytes" || r.Usage.PeakFileAllocated <= r.Budget.FileBytes || r.Worker.Signal != 9 {
		t.Fatalf("real file budget stop missing: %v %+v", err, r)
	}
	if _, err := os.Lstat(r.WorkRoot); !os.IsNotExist(err) {
		t.Fatal("test would have to delete the first work root")
	}
	firstRoot := r.WorkRoot
	q = Request{Kind: GoResource, Mode: FileSmall, Budget: fileNativeBudget(64<<10, 0, 3*time.Second)}
	r, err = s.Run(ctx, q)
	qaSupervisorRecord(p, q, r, err)
	qaResourceTerminal(t, cfg, r)
	if err != nil || r.State != Completed || r.Worker.ExitCode != 0 || r.WorkRoot == firstRoot || r.Usage.PeakFileAllocated > r.Budget.FileBytes || r.Usage.PeakFileAllocated == 0 {
		t.Fatalf("post-limit file recovery failed: %v %+v", err, r)
	}
	if _, err := os.Lstat(r.WorkRoot); !os.IsNotExist(err) {
		t.Fatal("recovered work root was still present")
	}
	prior := map[string]bool{}
	for _, call := range p.Calls {
		prior[call.Result.EvidenceDir] = true
	}
	callCtx, callCancel := context.WithCancel(ctx)
	q = Request{Kind: GoResource, Mode: FileHold, Budget: fileNativeBudget(64<<10, 0, 5*time.Second)}
	done := qaSupervisorStart(t, s, callCtx, q)
	qaSupervisorWaitEntry(t, cfg.EvidenceRoot, 1, prior, done)
	callCancel()
	a := <-done
	qaSupervisorRecord(p, q, a.r, a.err)
	qaResourceTerminal(t, cfg, a.r)
	if !errors.Is(a.err, context.Canceled) || errors.Is(a.err, ErrFileLimit) || errors.Is(a.err, ErrWallLimit) || a.r.State != Cancelled || a.r.StopReason != "cancel" {
		t.Fatalf("explicit cancellation became a file or wall stop: %v %+v", a.err, a.r)
	}
	q = Request{Kind: GoResource, Mode: FileSmall, Budget: fileNativeBudget(64<<10, 0, 3*time.Second)}
	r, err = s.Run(ctx, q)
	qaSupervisorRecord(p, q, r, err)
	qaResourceTerminal(t, cfg, r)
	if err != nil || r.Worker.ExitCode != 0 || r.WorkRoot == firstRoot {
		t.Fatalf("post-cancel recovery failed: %v %+v", err, r)
	}
	before, _ := json.Marshal(r)
	callCancel()
	after, _ := json.Marshal(r)
	if string(before) != string(after) {
		t.Fatal("late cancellation mutated a returned value")
	}
	afterCWD, err := os.Getwd()
	if err != nil || afterCWD != cwd || !reflect.DeepEqual(environment, os.Environ()) || !reflect.DeepEqual(limits, qaSupervisorLimits(t)) {
		t.Fatal("file Run polluted the host cwd, environment or rlimits")
	}
	p.Observations["test_did_not_remove_work_root"] = true
	p.Observations["host_cwd_environment_rlimits_unchanged"] = true
	p.Observations["post_limit_and_cancel_recovery"] = true
}

func TestFileQANativeConcurrentIsolation(t *testing.T) {
	p := qaFileNativeProof(t)
	s, cfg := qaSupervisorNative(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	holdCtx, holdCancel := context.WithCancel(ctx)
	defer holdCancel()
	holdQ := Request{Kind: GoResource, Mode: FileHold, Budget: fileNativeBudget(64<<10, 0, 5*time.Second)}
	growQ := Request{Kind: GoResource, Mode: FileGrow, Budget: fileNativeBudget(48<<10, 0, 3*time.Second)}
	holdDone := qaSupervisorStart(t, s, holdCtx, holdQ)
	growDone := qaSupervisorStart(t, s, ctx, growQ)
	qaSupervisorWaitEntry(t, cfg.EvidenceRoot, 2, nil, holdDone, growDone)
	holdCancel()
	hold, grow := <-holdDone, <-growDone
	qaSupervisorRecord(p, holdQ, hold.r, hold.err)
	qaSupervisorRecord(p, growQ, grow.r, grow.err)
	qaResourceTerminal(t, cfg, hold.r)
	qaResourceTerminal(t, cfg, grow.r)
	if !errors.Is(hold.err, context.Canceled) || !errors.Is(grow.err, ErrFileLimit) || grow.r.StopReason != "file_limit" || grow.r.Usage.FileTriggerAxis != "allocated_bytes" || errors.Is(grow.err, context.Canceled) {
		t.Fatalf("concurrent file reasons crossed: %v / %v", hold.err, grow.err)
	}
	if hold.r.Nonce == grow.r.Nonce || hold.r.Task.PID == grow.r.Task.PID || hold.r.WorkRoot == grow.r.WorkRoot || hold.r.EvidenceDir == grow.r.EvidenceDir {
		t.Fatal("concurrent file calls shared identity or directories")
	}
	for _, call := range []Result{hold.r, grow.r} {
		if _, err := os.Lstat(call.WorkRoot); !os.IsNotExist(err) {
			t.Fatalf("concurrent work root leaked: %s %v", call.WorkRoot, err)
		}
		for _, sample := range call.Usage.Samples {
			if sample.PID != call.Task.PID {
				t.Fatal("file sample was attributed to the other call")
			}
		}
	}
	p.Observations["independent_cancel_and_file_stop"] = true
	p.Observations["test_did_not_remove_work_root"] = true
}
