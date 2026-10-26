//go:build darwin

package supervisor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
)

type qaResourceCounterSample struct {
	Identity Identity `json:"identity"`
	User     uint64   `json:"raw_mach_user"`
	System   uint64   `json:"raw_mach_system"`
	LiveUser uint64   `json:"raw_mach_live_threads_user"`
	LiveSys  uint64   `json:"raw_mach_live_threads_system"`
	Numer    uint32   `json:"timebase_numer"`
	Denom    uint32   `json:"timebase_denom"`
}

func (s qaResourceCounterSample) cpuNS() uint64 {
	return uint64(float64(s.User+s.System) * float64(s.Numer) / float64(s.Denom))
}

func qaResourceCounterSampleSelf(t *testing.T) qaResourceCounterSample {
	t.Helper()
	m, err := monitor()
	if err != nil {
		t.Fatal(err)
	}
	id, err := m.identity(os.Getpid(), "private-fixed-counter-probe")
	if err != nil {
		t.Fatal(err)
	}
	var info struct {
		Virtual, Resident, User, System, ThreadsUser, ThreadsSystem uint64
		Counters                                                    [12]int32
	}
	if n := m.info(int32(os.Getpid()), 4, 0, unsafe.Pointer(&info), int32(unsafe.Sizeof(info))); n != int32(unsafe.Sizeof(info)) {
		t.Fatalf("native task CPU read was not complete: got=%d want=%d", n, unsafe.Sizeof(info))
	}
	handle, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		t.Fatal(err)
	}
	var timebase func(unsafe.Pointer) int32
	purego.RegisterLibFunc(&timebase, handle, "mach_timebase_info")
	var tb struct{ Numer, Denom uint32 }
	if code := timebase(unsafe.Pointer(&tb)); code != 0 || tb.Numer == 0 || tb.Denom == 0 {
		t.Fatalf("invalid native Mach timebase: code=%d numer=%d denom=%d", code, tb.Numer, tb.Denom)
	}
	return qaResourceCounterSample{Identity: id, User: info.User, System: info.System, LiveUser: info.ThreadsUser, LiveSys: info.ThreadsSystem, Numer: tb.Numer, Denom: tb.Denom}
}

func qaResourceCounterFixtureDir(t *testing.T, role string) string {
	t.Helper()
	run, out := flag.Lookup("test.run"), flag.Lookup("test.outputdir")
	if run == nil || out == nil || run.Value.String() != "^"+role+"$" {
		t.Skip("private bounded counter fixture, invoked only by its native parent")
	}
	path := out.Value.String()
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		t.Fatal("private fixture requires an absolute owned output directory")
	}
	if st, err := os.Lstat(path); err != nil || !st.IsDir() {
		t.Fatalf("private fixture output is not an existing owned directory: %v", err)
	}
	return path
}

func qaResourceCounterWrite(t *testing.T, path string, value any) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal("refuse existing native counter evidence: ", err)
	}
	if err := json.NewEncoder(f).Encode(value); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestResourceCounterPreExecFixture(t *testing.T) {
	out := qaResourceCounterFixtureDir(t, "TestResourceCounterPreExecFixture")
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	start := qaResourceCounterSampleSelf(t)
	deadline := time.Now().Add(3 * time.Second)
	var value uint64 = 123
	var before qaResourceCounterSample
	for {
		for i := 0; i < 1<<18; i++ {
			value = value*6364136223846793005 + 1
		}
		before = qaResourceCounterSampleSelf(t)
		if before.cpuNS() >= start.cpuNS()+uint64(200*time.Millisecond) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("native positive control did not accumulate 200ms measured CPU; wall sleep is not equivalent")
		}
	}
	runtime.KeepAlive(value)
	qaResourceCounterWrite(t, filepath.Join(out, "before.json"), map[string]any{"start": start, "before": before, "burn_cpu_ns": before.cpuNS() - start.cpuNS(), "phase": "private-preexec-go-test-after-measured-burn"})
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// This is the same private Darwin spawn/SETEXEC primitive as the sealed
	// launcher, with fixed test roles. No production helper/API mode is added.
	_, err = spawnFixed(canonical(exe), []string{"-test.run=^TestResourceCounterPostExecFixture$", "-test.outputdir=" + out, "-test.v"}, []*os.File{os.Stdin, os.Stdout, os.Stderr}, true)
	t.Fatalf("native SETEXEC returned instead of replacing the owned fixture: %v", err)
}

func TestResourceCounterPostExecFixture(t *testing.T) {
	out := qaResourceCounterFixtureDir(t, "TestResourceCounterPostExecFixture")
	// Capture before reading any preexec audit file. This is the first sample
	// after the Go test runtime has initialized, not the instant of exec itself.
	after := qaResourceCounterSampleSelf(t)
	time.Sleep(100 * time.Millisecond)
	hold := qaResourceCounterSampleSelf(t)
	qaResourceCounterWrite(t, filepath.Join(out, "after.json"), map[string]any{"after": after, "after_hold": hold, "phase": "private-postexec-go-test-first-sample-then-100ms-hold"})
}

func TestResourceCounterNativeQA(t *testing.T) {
	if os.Getenv("DARWIN_RESOURCE_COUNTER_QA") != "1" {
		t.Skip("requires explicit Darwin raw CPU counter native gate")
	}
	out := os.Getenv("DARWIN_RESOURCE_COUNTER_OUT")
	if !filepath.IsAbs(out) || filepath.Clean(out) != out {
		t.Fatal("DARWIN_RESOURCE_COUNTER_OUT must be a new absolute clean path")
	}
	if err := os.Mkdir(out, 0700); err != nil {
		t.Fatal("counter output must be exclusively created before any actor: ", err)
	}
	report := map[string]any{"schema": "lantern.darwin-resource-counter.v1", "status": "failed", "goos": runtime.GOOS, "goarch": runtime.GOARCH, "go_version": runtime.Version(), "preexec_burn_thread_locked": true, "thread_fields_scope": "PROC_PIDTASKINFO live-thread aggregate, not a single calling thread", "purpose": "native measurement experiment only; not enforcement or proof of an entire startup chain"}
	defer func() {
		if t.Failed() {
			report["status"] = "failed"
		}
		qaResourceCounterWrite(t, filepath.Join(out, "report.json"), report)
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	command := func(name string, args ...string) string {
		b, err := exec.CommandContext(ctx, name, args...).Output()
		if err != nil {
			t.Fatalf("native source/environment metadata unavailable: %v", err)
		}
		return strings.TrimSpace(string(b))
	}
	report["source_commit"] = command("git", "rev-parse", "HEAD")
	report["source_dirty"] = command("git", "status", "--porcelain") != ""
	report["macos_version"] = command("/usr/bin/sw_vers", "-productVersion")
	report["macos_build"] = command("/usr/bin/sw_vers", "-buildVersion")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bytes, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(bytes)
	report["actual_test_executable_sha256"] = hex.EncodeToString(digest[:])
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestResourceCounterPreExecFixture$", "-test.outputdir="+out, "-test.v")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C", "GOMAXPROCS=2"}
	log, err := cmd.CombinedOutput()
	if writeErr := os.WriteFile(filepath.Join(out, "child.log"), log, 0600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if err != nil || ctx.Err() != nil {
		report["error"] = fmt.Sprint(err)
		t.Fatalf("native counter experiment did not complete: %v %s", err, log)
	}
	var pre struct {
		Start  qaResourceCounterSample `json:"start"`
		Before qaResourceCounterSample `json:"before"`
		Burn   uint64                  `json:"burn_cpu_ns"`
	}
	var post struct {
		After qaResourceCounterSample `json:"after"`
		Hold  qaResourceCounterSample `json:"after_hold"`
	}
	read := func(name string, value any) {
		b, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, value); err != nil {
			t.Fatal(err)
		}
	}
	read("before.json", &pre)
	read("after.json", &post)
	if pre.Burn < uint64(200*time.Millisecond) || pre.Before.cpuNS() < pre.Start.cpuNS()+uint64(200*time.Millisecond) || pre.Before.Identity.PID == os.Getpid() || pre.Before.Identity.PID != cmd.ProcessState.Pid() {
		t.Fatal("preexec CPU burn or actual child identity positive control failed")
	}
	if pre.Before.Identity != post.After.Identity || post.After.Identity != post.Hold.Identity || pre.Before.Numer != post.After.Numer || pre.Before.Denom != post.After.Denom || post.Hold.cpuNS() < post.After.cpuNS() {
		t.Fatal("native SETEXEC identity/timebase/after-hold evidence is inconsistent")
	}
	report["before"], report["after"], report["after_hold"] = pre.Before, post.After, post.Hold
	report["measured_preexec_burn_cpu_ns"] = pre.Burn
	report["before_cpu_ns"], report["after_cpu_ns"] = pre.Before.cpuNS(), post.After.cpuNS()
	report["cpu_reset_observed"] = post.After.cpuNS() < pre.Before.cpuNS()
	report["same_pid_birth_image"] = true
	report["actual_child_exit_code"] = cmd.ProcessState.ExitCode()
	report["counter_scope_inference"] = "reset observed across SETEXEC; current-task counters do not cover the full preexec startup chain"
	if post.After.cpuNS() >= pre.Before.cpuNS() {
		report["counter_scope_inference"] = "no reset observed in this bounded probe; this alone does not prove all-thread full startup-chain preservation"
	}
	report["status"] = "passed"
	t.Logf("actual native counter reset=%v before=%dns after=%dns", report["cpu_reset_observed"], pre.Before.cpuNS(), post.After.cpuNS())
}
