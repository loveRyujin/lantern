//go:build linux && amd64

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/pyrt"
	"github.com/loveRyujin/lantern/internal/eval"
	"golang.org/x/sys/unix"
)

func TestSandboxRealQARuntimeConfigIsCompleteAndBounded(t *testing.T) {
	runtime := pyrt.Runtime{Venv: "/trusted/pyenv/venv", Python: "/trusted/pyenv/venv/bin/python", Base: pyrt.Interpreter{Path: "/trusted/base/python3", Prefix: "/trusted/base", Stdlib: "/trusted/base/lib/python3.13", Version: pyrt.Version{Major: 3, Minor: 13, Micro: 17}}}
	body, err := json.Marshal(runtime)
	if err != nil {
		t.Fatal(err)
	}
	valid := filepath.Join(t.TempDir(), "valid.json")
	if err := os.WriteFile(valid, body, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := readRuntime(valid)
	if err != nil || got == nil || !reflect.DeepEqual(*got, runtime) {
		t.Fatalf("runtime JSON roundtrip: got=%+v error=%v", got, err)
	}
	for name, data := range map[string][]byte{
		"trailing-json":        append(append([]byte{}, body...), []byte(" {}")...),
		"hidden-after-limit":   append(append(append([]byte{}, body...), bytes.Repeat([]byte(" "), 64<<10)...), []byte("{}")...),
		"oversized-whitespace": append(append([]byte{}, body...), bytes.Repeat([]byte(" "), 64<<10)...),
		"unknown-field":        []byte(`{"Venv":"/tmp/x","permission":"network"}`),
		"null":                 []byte("null"),
		"array":                []byte("[]"),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "runtime.json")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if got, err := readRuntime(path); err == nil || got != nil {
				t.Fatalf("incomplete/unbounded config accepted: got=%+v error=%v", got, err)
			}
		})
	}
	link := filepath.Join(t.TempDir(), "runtime-link")
	if err := os.Symlink(valid, link); err != nil {
		t.Fatal(err)
	}
	if got, err := readRuntime(link); err == nil || got != nil {
		t.Fatalf("symlink runtime config accepted: got=%+v error=%v", got, err)
	}
	fifo := filepath.Join(t.TempDir(), "runtime-fifo")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := readRuntime(fifo); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO runtime config accepted")
		}
	case <-time.After(time.Second):
		// Unblock an accidentally blocking FIFO read before failing this target
		// assertion; never count the global go-test timeout as a killed mutant.
		if fd, err := unix.Open(fifo, unix.O_WRONLY|unix.O_NONBLOCK, 0); err == nil {
			_, _ = unix.Write(fd, []byte("{}"))
			_ = unix.Close(fd)
		}
		t.Fatal("runtime config reader blocked on FIFO")
	}
}

func TestSandboxRealQAExplicitPreparationNeverOverwritesConfig(t *testing.T) {
	root := filepath.Join(t.TempDir(), "must-not-be-prepared")
	out := filepath.Join(t.TempDir(), "existing.json")
	if err := os.WriteFile(out, []byte("operator-existing-config"), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"prepare-runtime", "--root", root, "--out", out}, &stdout, &stderr)
	if code != eval.ExitUsage || !strings.Contains(stderr.String(), "不会覆盖") {
		t.Fatalf("existing config did not stop preparation: exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("rejected preparation created runtime root: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil || string(data) != "operator-existing-config" {
		t.Fatalf("existing config changed: %q %v", data, err)
	}
	runtime := pyrt.Runtime{Venv: "/trusted/pyenv/venv", Python: "/trusted/pyenv/venv/bin/python"}
	if err := writeNewRuntime(out, runtime); err == nil {
		t.Fatal("atomic writer overwrote existing config")
	}
	if data, err := os.ReadFile(out); err != nil || string(data) != "operator-existing-config" {
		t.Fatalf("writer changed existing config: %q %v", data, err)
	}
	fresh := filepath.Join(t.TempDir(), "runtime.json")
	if err := writeNewRuntime(fresh, runtime); err != nil {
		t.Fatal(err)
	}
	loaded, err := readRuntime(fresh)
	if err != nil || loaded == nil || !reflect.DeepEqual(*loaded, runtime) {
		t.Fatalf("published config was incomplete: got=%+v error=%v", loaded, err)
	}
	info, err := os.Stat(fresh)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("runtime config mode=%v error=%v", info, err)
	}
	for _, parent := range []string{filepath.Dir(out), filepath.Dir(fresh)} {
		if scratch, err := filepath.Glob(filepath.Join(parent, ".runtime-json-*")); err != nil || len(scratch) != 0 {
			t.Fatalf("config writer left scratch: %v error=%v", scratch, err)
		}
	}
}
