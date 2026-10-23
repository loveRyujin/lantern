//go:build unix

package pyrt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const preparedFakeSource = `#!/bin/sh
if [ "$1" != '-I' ] || [ "$2" != '-B' ] || [ "$3" != '-c' ]; then exit 71; fi
if [ -n "${LANTERN_PREPARED_SECRET-}" ] || [ -n "${PIP_INDEX_URL-}" ]; then exit 72; fi
if [ -f "$0.log-path" ]; then
 log=$(cat "$0.log-path")
 printf '%s\n' "$0 $*" >> "$log"
fi
if [ -f "$0.sleep" ]; then sleep 30; fi
case "$4" in
 *'import pandas,matplotlib'*)
  if [ -f "$0.import-fail" ]; then echo 'controlled import failure' >&2; exit 23; fi
  cat "$0.packages.json";;
 *) cat "$0.metadata.json";;
esac
`

func preparedFixture(t *testing.T) (Runtime, string, string) {
	t.Helper()
	root := t.TempDir()
	baseDir := filepath.Join(root, "base-python")
	stdlib := filepath.Join(baseDir, "lib/python3.12")
	venv := filepath.Join(root, "pyenv/venv")
	py := venvPython(venv)
	base := filepath.Join(baseDir, "python3")
	for _, p := range []string{stdlib, filepath.Dir(py)} {
		if err := os.MkdirAll(p, 0755); err != nil {
			t.Fatal(err)
		}
	}
	rt := Runtime{Base: Interpreter{Path: base, Prefix: baseDir, Stdlib: stdlib, Version: Version{3, 12, 1}}, Venv: venv, Python: py}
	log := filepath.Join(t.TempDir(), "calls.log")
	for _, p := range []string{base, py} {
		preparedWrite(t, p, []byte(preparedFakeSource), 0755)
		preparedWrite(t, p+".log-path", []byte(log), 0600)
	}
	preparedJSON(t, base+".metadata.json", preparedProbe{base, base, baseDir, baseDir, stdlib, []int{3, 12, 1}})
	preparedJSON(t, py+".metadata.json", preparedProbe{py, base, venv, baseDir, stdlib, []int{3, 12, 1}})
	spec, err := embeddedLock()
	if err != nil {
		t.Fatal(err)
	}
	packages := preparedPackages{Versions: map[string]string{}, Distributions: map[string]string{}, Modules: map[string]string{}}
	site := filepath.Join(venv, "lib/python3.12/site-packages")
	if err := os.MkdirAll(site, 0755); err != nil {
		t.Fatal(err)
	}
	for _, p := range spec.packages {
		packages.Versions[p.Name] = p.Version
		packages.Distributions[p.Name] = site
	}
	for _, pkg := range spec.packages {
		name := pkg.Name
		path := filepath.Join(site, name, "__init__.py")
		preparedWrite(t, path, []byte("# trusted fixture package"), 0600)
		packages.Modules[name] = path
	}
	preparedJSON(t, py+".packages.json", packages)
	sum := sha256.Sum256(requirementsLock)
	preparedWrite(t, filepath.Join(venv, markerName), formatMarker(hex.EncodeToString(sum[:]), rt.Base), 0600)
	preparedWrite(t, filepath.Join(root, ensureLockName), nil, 0600)
	preparedWrite(t, filepath.Join(root, "pyenv/.venv-old-keep/sentinel"), []byte("never sweep during validation"), 0600)
	return rt, root, log
}
func preparedWrite(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}
func preparedJSON(t *testing.T, path string, value any) {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	preparedWrite(t, path, body, 0600)
}
func preparedUnavailable(t *testing.T, release func(), err error) {
	t.Helper()
	if release != nil {
		release()
		t.Fatal("failure returned a lease")
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want runtime_unavailable: %v", err)
	}
}
func TestPreparedSharedLeaseIsOfflineAndReadOnly(t *testing.T) {
	rt, root, log := preparedFixture(t)
	t.Setenv("PIP_INDEX_URL", "https://invalid.invalid/no-install")
	t.Setenv("LANTERN_PREPARED_SECRET", "must-not-enter-probe")
	before := snapshotTree(t, root)
	release, err := AcquirePrepared(t.Context(), rt)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	second, err := AcquirePrepared(t.Context(), rt)
	if err != nil {
		t.Fatal(err)
	}
	if unlock, err := lockEnsure(root); err == nil {
		unlock()
		t.Fatal("shared validation lease did not exclude Ensure")
	}
	release()
	release() // closing a lease twice must not close another process's fd.
	if unlock, err := lockEnsure(root); err == nil {
		unlock()
		t.Fatal("second shared lease was accidentally released")
	}
	second()
	unlock, err := lockEnsure(root)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if after := snapshotTree(t, root); after != before {
		t.Fatal("validation modified its prepared root")
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(calls), " -m ") || !strings.Contains(string(calls), "-I -B -c") || strings.Count(string(calls), "import pandas,matplotlib") != 2 {
		t.Fatalf("unexpected commands: %s", calls)
	}
}
func TestPreparedRejectsDamageWithoutRepair(t *testing.T) {
	for _, name := range []string{"missing-marker", "oversize-marker", "bad-hash", "bad-base", "bad-version", "bad-prefix", "bad-stdlib", "wrong-packages", "outside-distribution", "outside-module", "outside-dependency-module", "symlink-distribution", "symlink-module", "symlink-dependency-module", "imports-fail", "missing-lock", "bad-python", "unrelated-runtime"} {
		t.Run(name, func(t *testing.T) {
			rt, root, _ := preparedFixture(t)
			marker := filepath.Join(rt.Venv, markerName)
			switch name {
			case "missing-marker":
				if err := os.Remove(marker); err != nil {
					t.Fatal(err)
				}
			case "oversize-marker":
				preparedWrite(t, marker, []byte(strings.Repeat("x", 8193)), 0600)
			case "bad-hash":
				preparedWrite(t, marker, formatMarker(strings.Repeat("0", 64), rt.Base), 0600)
			case "bad-base":
				changed := rt.Base
				changed.Path = "/unrelated/python"
				sum := sha256.Sum256(requirementsLock)
				preparedWrite(t, marker, formatMarker(hex.EncodeToString(sum[:]), changed), 0600)
			case "bad-version":
				rt.Base.Version = Version{3, 14, 0}
			case "bad-prefix":
				preparedJSON(t, rt.Python+".metadata.json", preparedProbe{rt.Python, rt.Base.Path, rt.Base.Prefix, rt.Base.Prefix, rt.Base.Stdlib, []int{3, 12, 1}})
			case "bad-stdlib":
				preparedJSON(t, rt.Python+".metadata.json", preparedProbe{rt.Python, rt.Base.Path, rt.Venv, rt.Base.Prefix, root, []int{3, 12, 1}})
			case "wrong-packages":
				preparedJSON(t, rt.Python+".packages.json", map[string]string{"pandas": "0.0"})
			case "outside-distribution", "outside-module", "outside-dependency-module", "symlink-distribution", "symlink-module", "symlink-dependency-module":
				body, err := os.ReadFile(rt.Python + ".packages.json")
				if err != nil {
					t.Fatal(err)
				}
				var packages preparedPackages
				if err = json.Unmarshal(body, &packages); err != nil {
					t.Fatal(err)
				}
				switch name {
				case "outside-distribution":
					packages.Distributions["numpy"] = rt.Base.Stdlib
				case "outside-module":
					packages.Modules["pandas"] = rt.Base.Path
				case "outside-dependency-module":
					packages.Modules["numpy"] = rt.Base.Path
				case "symlink-distribution":
					alias := filepath.Join(rt.Venv, "external-site")
					if err := os.Symlink(rt.Base.Stdlib, alias); err != nil {
						t.Fatal(err)
					}
					packages.Distributions["numpy"] = alias
				case "symlink-module", "symlink-dependency-module":
					alias := filepath.Join(rt.Venv, "external-module.py")
					if err := os.Symlink(rt.Base.Path, alias); err != nil {
						t.Fatal(err)
					}
					module := "pandas"
					if name == "symlink-dependency-module" {
						module = "numpy"
					}
					packages.Modules[module] = alias
				}
				preparedJSON(t, rt.Python+".packages.json", packages)
			case "imports-fail":
				preparedWrite(t, rt.Python+".import-fail", nil, 0600)
			case "missing-lock":
				if err := os.Remove(filepath.Join(root, ensureLockName)); err != nil {
					t.Fatal(err)
				}
			case "bad-python":
				preparedWrite(t, rt.Python, []byte("#!/bin/sh\nexit 0\n"), 0755)
			case "unrelated-runtime":
				rt.Python = rt.Base.Path
			}
			before := snapshotTree(t, root)
			release, err := AcquirePrepared(t.Context(), rt)
			preparedUnavailable(t, release, err)
			if snapshotTree(t, root) != before {
				t.Fatal("invalid prepared runtime was repaired or swept")
			}
		})
	}
}
func TestPreparedRejectsSymlinkBoundaries(t *testing.T) {
	for _, name := range []string{"pyenv", "venv", "bin", "marker", "lock"} {
		t.Run(name, func(t *testing.T) {
			rt, root, _ := preparedFixture(t)
			path := map[string]string{"pyenv": filepath.Join(root, "pyenv"), "venv": rt.Venv, "bin": filepath.Dir(rt.Python), "marker": filepath.Join(rt.Venv, markerName), "lock": filepath.Join(root, ensureLockName)}[name]
			target := path + "-outside"
			if err := os.Rename(path, target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
			before := snapshotTree(t, root)
			release, err := AcquirePrepared(t.Context(), rt)
			preparedUnavailable(t, release, err)
			if snapshotTree(t, root) != before {
				t.Fatal("validation followed/modified a forbidden symlink")
			}
		})
	}
}
func TestPreparedBusyAndCancellationRelease(t *testing.T) {
	rt, root, log := preparedFixture(t)
	unlock, err := lockEnsure(root)
	if err != nil {
		t.Fatal(err)
	}
	release, err := AcquirePrepared(t.Context(), rt)
	preparedUnavailable(t, release, err)
	unlock()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	release, err = AcquirePrepared(ctx, rt)
	if release != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled release=%v error=%v", release != nil, err)
	}
	preparedWrite(t, rt.Python+".sleep", nil, 0600)
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		release, err := AcquirePrepared(ctx, rt)
		if release != nil {
			release()
		}
		done <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		body, _ := os.ReadFile(log)
		if strings.Contains(string(body), rt.Python) {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("fixed metadata probe never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err = <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled validation did not return")
	}
	unlock, err = lockEnsure(root)
	if err != nil {
		t.Fatalf("cancellation leaked lease: %v", err)
	}
	unlock()
}
func TestPreparedReal(t *testing.T) {
	if os.Getenv("PYRT_PREPARED_REAL") != "1" {
		t.Skip("PYRT_PREPARED_REAL=1 enables trusted setup and real offline validation")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	root := t.TempDir()
	rt, err := (Config{Root: root, Python: python, SearchPaths: []string{}}).Ensure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	before := preparedTreeMetadata(t, root)
	t.Setenv("PIP_INDEX_URL", "https://invalid.invalid/no-install")
	t.Setenv("LANTERN_PREPARED_SECRET", "must-not-enter-probe")
	release, err := AcquirePrepared(ctx, rt)
	if err != nil {
		t.Fatal(err)
	}
	if unlock, err := lockEnsure(root); err == nil {
		unlock()
		release()
		t.Fatal("real lease allowed concurrent Ensure")
	}
	release()
	if err = ValidatePrepared(ctx, rt); err != nil {
		t.Fatal(err)
	}
	if after := preparedTreeMetadata(t, root); after != before {
		t.Fatal("offline validation changed prepared environment files")
	}
	t.Logf("real prepared Python %s: marker/version/imports/read-only lease verified", rt.Base.Version.String())
}
func preparedTreeMetadata(t *testing.T, root string) string {
	t.Helper()
	hash := sha256.New()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		fmt.Fprintf(hash, "%s %s %d %d\n", rel, info.Mode(), info.Size(), info.ModTime().UnixNano())
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			fmt.Fprintln(hash, target)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func TestPreparedRejectsSpecialLockWithoutBlocking(t *testing.T) {
	for _, kind := range []string{"fifo", "directory"} {
		t.Run(kind, func(t *testing.T) {
			rt, root, _ := preparedFixture(t)
			lock := filepath.Join(root, ensureLockName)
			if err := os.Remove(lock); err != nil {
				t.Fatal(err)
			}
			if kind == "fifo" {
				if err := syscall.Mkfifo(lock, 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(lock, 0700); err != nil {
					t.Fatal(err)
				}
			}
			started := time.Now()
			release, err := AcquirePrepared(t.Context(), rt)
			preparedUnavailable(t, release, err)
			if time.Since(started) > time.Second {
				t.Fatal("special lock did not fail promptly")
			}
		})
	}
}

func TestPreparedWorksWithReadOnlyEnvironment(t *testing.T) {
	rt, root, _ := preparedFixture(t)
	dirs := []string{root, filepath.Join(root, "pyenv"), rt.Venv, filepath.Dir(rt.Python)}
	for _, dir := range dirs {
		if err := os.Chmod(dir, 0555); err != nil {
			t.Fatal(err)
		}
	}
	lock := filepath.Join(root, ensureLockName)
	if err := os.Chmod(lock, 0444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, dir := range dirs {
			_ = os.Chmod(dir, 0755)
		}
		_ = os.Chmod(lock, 0600)
	})
	before := snapshotTree(t, root)
	if err := ValidatePrepared(t.Context(), rt); err != nil {
		t.Fatal(err)
	}
	if after := snapshotTree(t, root); after != before {
		t.Fatal("validation changed a read-only prepared root")
	}
}
