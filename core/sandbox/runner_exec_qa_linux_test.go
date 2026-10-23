//go:build linux && amd64

package sandbox_test

import (
	"debug/elf"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/errcode"
	"github.com/loveRyujin/lantern/core/sandbox"
)

func TestSandboxQAResourceMonitoringSurvivesLoaderExec(t *testing.T) {
	runner, root := blackboxRunner(t)
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	binary, err := elf.Open(python)
	if err != nil {
		t.Fatal(err)
	}
	defer binary.Close()
	loader := ""
	for _, segment := range binary.Progs {
		if segment.Type == elf.PT_INTERP {
			data := make([]byte, segment.Filesz)
			if _, err := segment.ReadAt(data, 0); err != nil {
				t.Fatal(err)
			}
			loader = strings.TrimRight(string(data), "\x00")
		}
	}
	if loader == "" {
		t.Fatal("configured ELF Python has no interpreter segment")
	}
	// Explicitly running ld.so is permitted by the same loader closure needed
	// for ordinary Python startup. It changes /proc/PID/exe without making a
	// second process: monitoring must follow the task rather than one pathname.
	t.Run("rss", func(t *testing.T) {
		code := fmt.Sprintf("import os,sys\nos.execv(%q,[%q,sys.executable,'-I','-B','-c','import time;x=bytearray(64<<20);time.sleep(10)'])", loader, loader)
		result, err := blackboxRun(t, runner, sandbox.Spec{Code: code, Limits: sandbox.Limits{Memory: 32 << 20}})
		blackboxCode(t, result, err, errcode.OOM)
		if result.Usage.Wall >= 3*time.Second || result.Usage.PeakRSS <= 32<<20 {
			t.Fatalf("loader exec bypassed live RSS enforcement; wait4 after the 10s sleep is insufficient: %+v", result.Usage)
		}
		blackboxReaped(t, result.Usage.PythonPID)
	})
	t.Run("cpu", func(t *testing.T) {
		code := fmt.Sprintf("import os,sys\nos.execv(%q,[%q,sys.executable,'-I','-B','-c','while True: pass'])", loader, loader)
		budget := 200 * time.Millisecond
		result, err := blackboxRun(t, runner, sandbox.Spec{Code: code, Limits: sandbox.Limits{CPU: budget, Wall: 8 * time.Second}})
		blackboxCode(t, result, err, errcode.Timeout)
		// An integer RLIMIT_CPU fallback can eventually terminate this loop;
		// it must not mask a missing fractional CPU-time sampler after exec.
		if result.Usage.SampleCount == 0 || result.Usage.CPU > budget+result.Usage.MaxSampleGap+100*time.Millisecond || result.Usage.Wall >= 3*time.Second {
			t.Fatalf("loader exec bypassed live CPU enforcement: %+v", result.Usage)
		}
		blackboxReaped(t, result.Usage.PythonPID)
	})
	blackboxClean(t, root)
}

func TestSandboxQARuntimeImpostorWithPlausibleConfig(t *testing.T) {
	runner, root := blackboxRunner(t)
	impostor := t.TempDir()
	// Without this file, a removed metadata equality check is accidentally
	// masked by a later missing-pyvenv.cfg error, giving a false green test.
	if err := os.WriteFile(filepath.Join(impostor, "pyvenv.cfg"), []byte("home = /usr/bin\ninclude-system-site-packages = false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := blackboxRun(t, runner, sandbox.Spec{Code: "print('impostor-runtime-accepted')", Runtime: sandbox.Runtime{Venv: impostor}})
	if !errors.Is(err, sandbox.ErrRuntimeUnavailable) || result.ErrKind != errcode.RuntimeUnavailable {
		t.Fatalf("plausible unrelated venv expanded runtime permissions: result=%+v error=%v", result, err)
	}
	blackboxClean(t, root)
}
