package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/pyrt"
)

func TestDarwinProbeQAOptionsRejectArbitraryExecution(t *testing.T) {
	for name, args := range map[string][]string{
		"empty":             nil,
		"missing-output":    {"--runtime-config", "rt.json"},
		"missing-runtime":   {"--out", "reports"},
		"code":              {"--runtime-config", "rt.json", "--out", "reports", "--code", "print('unsafe')"},
		"command":           {"--runtime-config", "rt.json", "--out", "reports", "--command", "/bin/sh"},
		"positional":        {"--runtime-config", "rt.json", "--out", "reports", "/bin/sh"},
		"after-double-dash": {"--runtime-config", "rt.json", "--out", "reports", "--", "/bin/sh"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseOptions(args); err == nil {
				t.Fatal("unexpected execution arguments accepted")
			}
		})
	}
	o, err := parseOptions([]string{"--runtime-config", "运行时 with spaces.json", "--out", "报告 with spaces", "--require-feasible"})
	if err != nil || o.RuntimeConfig != "运行时 with spaces.json" || o.Out != "报告 with spaces" || !o.RequireFeasible {
		t.Fatalf("valid fixed-experiment options changed: %+v %v", o, err)
	}
}

func TestDarwinProbeQARuntimeConfigIsCompleteAndBounded(t *testing.T) {
	root := t.TempDir()
	rt := pyrt.Runtime{}
	b, err := json.Marshal(rt)
	if err != nil {
		t.Fatal(err)
	}
	valid := filepath.Join(root, "运行时.json")
	if err := os.WriteFile(valid, append(b, bytes.Repeat([]byte{' '}, (64<<10)-len(b))...), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := readRuntime(valid)
	if err != nil || !reflect.DeepEqual(got, rt) {
		t.Fatalf("valid exact-64KiB config: %+v %v", got, err)
	}
	for name, body := range map[string]string{
		"null":                "null",
		"array":               "[]",
		"unknown-field":       `{"unregistered":true}`,
		"trailing":            string(b) + " {}",
		"oversize-whitespace": string(b) + strings.Repeat(" ", 64<<10),
		"hidden-after-limit":  string(b) + strings.Repeat(" ", 64<<10) + "{}",
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(root, name+".json")
			if err := os.WriteFile(p, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := readRuntime(p); err == nil {
				t.Fatal("incomplete/oversized config accepted")
			}
		})
	}
	if _, err := readRuntime(root); err == nil {
		t.Fatal("directory accepted as runtime config")
	}
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		link := filepath.Join(root, "runtime-link.json")
		if err := os.Symlink(valid, link); err != nil {
			t.Fatal(err)
		}
		if _, err := readRuntime(link); err == nil {
			t.Fatal("symlink runtime config accepted")
		}
	}
}

func TestDarwinProbeQAFIFOConfigDoesNotBlock(t *testing.T) {
	if path := os.Getenv("LANTERN_DARWIN_QA_FIFO"); path != "" {
		if _, err := readRuntime(path); err == nil {
			os.Exit(0)
		}
		os.Exit(9)
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return
	}
	path := filepath.Join(t.TempDir(), "config.fifo")
	if out, err := exec.Command("mkfifo", path).CombinedOutput(); err != nil {
		t.Fatalf("create owned FIFO: %s %v", out, err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestDarwinProbeQAFIFOConfigDoesNotBlock$")
	cmd.Env = append(os.Environ(), "LANTERN_DARWIN_QA_FIFO="+path)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatal("opening a FIFO blocked before regular-file validation")
	}
	e, ok := err.(*exec.ExitError)
	if !ok || e.ExitCode() != 9 {
		t.Fatalf("FIFO was not rejected: error=%v output=%s", err, out)
	}
}

// Invoke the real command entry point in an owned test subprocess so os.Exit,
// stdout and durable report evidence are checked together.
func TestDarwinProbeQACLIProcess(t *testing.T) {
	if body := os.Getenv("LANTERN_DARWIN_QA_CHILD_ARGS"); body != "" {
		var args []string
		if err := json.Unmarshal([]byte(body), &args); err != nil {
			panic(err)
		}
		os.Args = append([]string{"lantern-darwin-probe"}, args...)
		main()
		panic("command main returned without exit")
	}
	run := func(args []string) (int, []byte) {
		t.Helper()
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, exe, "-test.run=^TestDarwinProbeQACLIProcess$")
		b, _ := json.Marshal(args)
		cmd.Env = append(os.Environ(), "LANTERN_DARWIN_QA_CHILD_ARGS="+string(b))
		out, err := cmd.CombinedOutput()
		if ctx.Err() != nil {
			t.Fatalf("command hung: %s", out)
		}
		if err == nil {
			return 0, out
		}
		if e, ok := err.(*exec.ExitError); ok {
			return e.ExitCode(), out
		}
		t.Fatalf("command failed to start: %v", err)
		return -1, nil
	}
	t.Run("invalid-arguments-have-no-effects", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "must-not-exist")
		code, body := run([]string{"--runtime-config", "absent.json", "--out", out, "--code", "unsafe"})
		if code != 1 {
			t.Fatalf("invalid CLI exit=%d output=%s", code, body)
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Fatalf("invalid arguments created output: %v", err)
		}
	})
	if runtime.GOOS != "darwin" {
		t.Run("non-darwin-preserves-unsupported-report", func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "reports")
			code, body := run([]string{"--runtime-config", "absent.json", "--out", out})
			if code != 2 {
				t.Fatalf("non-Darwin exit=%d output=%s", code, body)
			}
			var report Report
			if err := json.Unmarshal(body, &report); err != nil || report.Status != "unsupported" || report.Production {
				t.Fatalf("unsupported stdout: %s error=%v", body, err)
			}
			if _, err := os.Stat(filepath.Join(out, "report.json")); err != nil {
				t.Fatalf("unsupported report not retained: %v", err)
			}
		})
	}
}
