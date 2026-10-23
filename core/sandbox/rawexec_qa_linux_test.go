//go:build linux && amd64

package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// This helper calls the actual terminal routine in a disposable process. No
// production hook or sandbox policy bypass is installed in the application.
func TestSandboxQARawTerminalHelper(t *testing.T) {
	mode := os.Getenv("LANTERN_QA_RAW_TERMINAL")
	if mode == "" {
		return
	}
	runtime.LockOSThread()
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		os.Exit(121)
	}
	python := os.Getenv("LANTERN_QA_RAW_PYTHON")
	marker := os.Getenv("LANTERN_QA_RAW_MARKER")
	plan, err := prepareRawLaunch(python, []string{python, "-I", "-B", "-c", "open(" + strconvQAQuote(marker) + ",'w').write('untrusted-ran')"}, []string{"PATH=/usr/bin:/bin"})
	if err != nil {
		os.Exit(122)
	}
	switch mode {
	case "invalid-filter":
		plan.program.Len = 0
	case "closed-ready-fd":
		_ = unix.Close(3)
	case "bad-exec":
		// The parent passes a nonexistent absolute filename.
	default:
		os.Exit(123)
	}
	rawLaunchTerminal(plan)
	os.Exit(124) // The terminal must never return, including on every error.
}

func strconvQAQuote(s string) string {
	// JSON strings are also valid Python literals for the ASCII temporary paths.
	b, _ := json.Marshal(s)
	return string(b)
}

func TestSandboxQARawTerminalFailurePaths(t *testing.T) {
	if err := checkUnprivileged(); err != nil {
		if os.Getenv("SANDBOX_REQUIRE_REAL") != "1" {
			t.Skipf("unprivileged terminal required: %v", err)
		}
		t.Fatal(err)
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"invalid-filter", "closed-ready-fd", "bad-exec"} {
		t.Run(mode, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "untrusted-code-marker")
			filename := python
			if mode == "bad-exec" {
				filename = filepath.Join(t.TempDir(), "missing-python")
			}
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			defer writer.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestSandboxQARawTerminalHelper$")
			// Match the production shim's async-preemption condition. Python
			// receives only the explicit environment built inside the helper.
			cmd.Env = append(os.Environ(), "GODEBUG=asyncpreemptoff=1", "LANTERN_QA_RAW_TERMINAL="+mode, "LANTERN_QA_RAW_PYTHON="+filename, "LANTERN_QA_RAW_MARKER="+marker)
			cmd.ExtraFiles = []*os.File{writer}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			writer.Close()
			err = cmd.Wait()
			var exit *exec.ExitError
			if ctx.Err() != nil || !errors.As(err, &exit) || exit.ExitCode() != 125 {
				t.Fatalf("terminal must exit125, not return/panic/hang: error=%v context=%v stderr=%s", err, ctx.Err(), stderr.String())
			}
			data, err := io.ReadAll(io.LimitReader(reader, 4096))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("untrusted code ran on terminal failure: %v", err)
			}
			if mode == "closed-ready-fd" {
				// A closed reporting FD cannot convey Error. The fail-closed
				// contract is EOF without Ready, exit125, and no user execution.
				if len(data) != 0 {
					t.Fatalf("unexpected report on closed fd: %s", data)
				}
				return
			}
			decoder := json.NewDecoder(bytes.NewReader(data))
			var messages []setupMessage
			for {
				var m setupMessage
				if err := decoder.Decode(&m); err == io.EOF {
					break
				} else if err != nil {
					t.Fatalf("invalid setup protocol: %q %v", data, err)
				}
				messages = append(messages, m)
			}
			if mode == "invalid-filter" {
				if len(messages) != 1 || messages[0].Ready || !strings.Contains(messages[0].Error, "seccomp installation failed") {
					t.Fatalf("filter failure protocol: %+v", messages)
				}
			} else if len(messages) != 2 || !messages[0].Ready || !strings.Contains(messages[1].Error, "Python exec failed") || messages[0].LauncherPeakRSS == 0 {
				t.Fatalf("exec failure must follow Ready with Error: %+v", messages)
			}
			if stderr.Len() != 0 {
				t.Fatalf("unexpected runtime diagnostic: %s", stderr.String())
			}
		})
	}
}
