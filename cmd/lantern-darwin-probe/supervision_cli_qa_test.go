package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func qaSupervisionCLI(t *testing.T, args []string) (int, []byte) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// Reuse the existing owned test process entry point; main itself dispatches
	// the new subcommand, including its real os.Exit and report-write path.
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestDarwinProbeQACLIProcess$")
	body, _ := json.Marshal(append([]string{"supervision"}, args...))
	cmd.Env = append(os.Environ(), "LANTERN_DARWIN_QA_CHILD_ARGS="+string(body))
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatal("supervision CLI did not finish its bounded argument/platform path")
	}
	if err == nil {
		return 0, out
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), out
	}
	t.Fatalf("supervision command failed to start: %v", err)
	return -1, nil
}

func TestSupervisionQACLIRejectsExecutionAndProductionFlags(t *testing.T) {
	for name, extra := range map[string][]string{
		"arbitrary-code":       {"--code", "print('unsafe')"},
		"arbitrary-command":    {"--command", "/bin/sh"},
		"arbitrary-positional": {"/bin/sh"},
		"production-feasible":  {"--require-feasible"},
	} {
		t.Run(name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "must-not-exist")
			args := append([]string{"--runtime-config", "absent.json", "--out", out}, extra...)
			code, body := qaSupervisionCLI(t, args)
			if code != 1 {
				t.Fatalf("invalid supervision arguments exit=%d body=%s", code, body)
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Fatalf("invalid arguments created experiment output: %v", err)
			}
		})
	}
}

func TestSupervisionQACLIKeepsUnavailableEvidence(t *testing.T) {
	if runtime.GOOS == "darwin" {
		return
	} // This assertion concerns the non-Darwin command, not native isolation.
	out := filepath.Join(t.TempDir(), "报告 with spaces")
	code, body := qaSupervisionCLI(t, []string{"--runtime-config", "absent.json", "--out", out})
	if code != 2 {
		t.Fatalf("unsupported supervision exit=%d body=%s", code, body)
	}
	var r SupervisionReport
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("unsupported stdout is not complete JSON: %v %s", err, body)
	}
	if r.Schema != "lantern.darwin-supervision.v1" || r.Status != "unsupported" || r.SingleFaultVerified || r.Production || r.Error == "" {
		t.Fatalf("unavailable platform made a supervision claim: %+v", r)
	}
	var saved SupervisionReport
	b, err := os.ReadFile(filepath.Join(out, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &saved); err != nil || saved.Schema != r.Schema || saved.Error != r.Error {
		t.Fatalf("durable unavailable evidence differs: %+v %v", saved, err)
	}
	md, err := os.ReadFile(filepath.Join(out, "report.md"))
	if err != nil || !strings.Contains(string(md), "unsupported") || !strings.Contains(string(md), "single-fault verified **false**") {
		t.Fatalf("Markdown concealed the unexecuted state: %s %v", md, err)
	}
}

func TestSupervisionQACLIRejectsIncompleteReportArtifacts(t *testing.T) {
	out := t.TempDir()
	// Keep report.json writable while making only Markdown fail. This must not
	// be mistaken for a successful run or merely a platform-unavailable result.
	if err := os.Mkdir(filepath.Join(out, "report.md"), 0700); err != nil {
		t.Fatal(err)
	}
	code, body := qaSupervisionCLI(t, []string{"--runtime-config", filepath.Join(out, "absent.json"), "--out", out})
	if code != 1 {
		t.Fatalf("missing mandatory report artifact exit=%d output=%s", code, body)
	}
	var stdout, saved SupervisionReport
	if err := json.Unmarshal(body, &stdout); err != nil {
		t.Fatalf("report-write failure stdout invalid: %s %v", body, err)
	}
	b, err := os.ReadFile(filepath.Join(out, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	for _, r := range []SupervisionReport{stdout, saved} {
		if r.Status != "failed" || r.SingleFaultVerified || r.Error == "" {
			t.Fatalf("incomplete report artifacts retained a verification claim: %+v", r)
		}
	}
}
