package pyrt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// AcquirePrepared validates an already trusted, prepared Runtime without
// installing, repairing, searching for alternatives, or sweeping runtime files.
// Its read-only shared lease excludes cooperating Ensure calls until release.
// The caller must hold the lease across Sandbox.Run and call release afterwards.
// All validation failures are runtime_unavailable; caller cancellation is preserved.
// The marker ties preparation to the embedded lock, not an attestation of every
// installed package byte against hostile modifications by the same OS user.
func AcquirePrepared(ctx context.Context, rt Runtime) (release func(), err error) {
	if ctx == nil {
		return nil, unavailable("prepared", "", "nil context")
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	fail := func(e error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return unavailable("prepared", rt.Venv, e.Error())
	}
	if !cleanAbsolute(rt.Venv) || filepath.Base(rt.Venv) != "venv" || filepath.Base(filepath.Dir(rt.Venv)) != "pyenv" || rt.Python != venvPython(rt.Venv) {
		return nil, fail(fmt.Errorf("runtime must be Root/pyenv/venv with its own interpreter"))
	}
	for _, path := range []string{rt.Base.Path, rt.Base.Prefix, rt.Base.Stdlib} {
		if !cleanAbsolute(path) {
			return nil, fail(fmt.Errorf("base interpreter metadata must contain clean absolute paths"))
		}
	}
	root, err := trustedRoot(filepath.Dir(filepath.Dir(rt.Venv)))
	if err != nil {
		return nil, fail(err)
	}
	if err = guardPreparedPaths(root, rt); err != nil {
		return nil, fail(err)
	}
	unlock, err := lockPrepared(root)
	if err != nil {
		return nil, fail(err)
	}
	success := false
	defer func() {
		if !success {
			unlock()
		}
	}()
	if err = guardPreparedPaths(root, rt); err != nil {
		return nil, fail(err)
	}
	markerPath := filepath.Join(rt.Venv, markerName)
	marker, err := readPreparedRegular(markerPath, 8<<10)
	if err != nil {
		return nil, fail(err)
	}
	spec, err := embeddedLock()
	if err != nil {
		return nil, fail(err)
	}
	hash := sha256.Sum256(requirementsLock)
	gotHash, gotBase, gotVersion, ok := parseMarker(string(marker))
	if !ok || gotHash != hex.EncodeToString(hash[:]) || gotBase != rt.Base.Path || gotVersion != rt.Base.Version.String() || !spec.supports(rt.Base.Version) {
		return nil, fail(fmt.Errorf("preparation marker/base/version does not match the current lock"))
	}
	// Reject a substituted executable before running its metadata probe. Unix
	// venvs normally symlink the base; byte-identical copied interpreters are also valid.
	if err = preparedExecutable(ctx, rt.Base.Path, rt.Python); err != nil {
		return nil, fail(err)
	}
	scratch, err := os.MkdirTemp("", "lantern-prepared-check-")
	if err != nil {
		return nil, fail(err)
	}
	defer os.RemoveAll(scratch)
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	env := preparedEnv(scratch)
	base, err := preparedMetadata(probeCtx, env, rt.Base.Path)
	if err != nil {
		return nil, fail(err)
	}
	venv, err := preparedMetadata(probeCtx, env, rt.Python)
	if err != nil {
		return nil, fail(err)
	}
	if !samePreparedPath(base.Executable, rt.Base.Path) || !samePreparedPath(base.Prefix, rt.Base.Prefix) || !samePreparedPath(base.BasePrefix, rt.Base.Prefix) || !samePreparedPath(base.Stdlib, rt.Base.Stdlib) || base.Version != rt.Base.Version {
		return nil, fail(fmt.Errorf("base Python metadata changed"))
	}
	if !samePreparedPath(venv.Executable, rt.Python) || !samePreparedPath(venv.Prefix, rt.Venv) || !samePreparedPath(venv.BasePrefix, rt.Base.Prefix) || !samePreparedPath(venv.BaseExecutable, rt.Base.Path) || !samePreparedPath(venv.Stdlib, rt.Base.Stdlib) || venv.Version != rt.Base.Version {
		return nil, fail(fmt.Errorf("virtual-environment metadata does not match the configured runtime"))
	}
	names := make([]string, len(spec.packages))
	for i, pkg := range spec.packages {
		names[i] = pkg.Name
	}
	arguments := append([]string{"-I", "-B", "-c", preparedImportSnippet}, names...)
	result, err := runCmdEnv(probeCtx, env, rt.Python, arguments...)
	if err != nil {
		return nil, fail(fmt.Errorf("prepared imports: %w (%s)", err, trimDetail(firstOutput(result))))
	}
	var installed preparedPackages
	if json.Unmarshal(bytes.TrimSpace(result.Stdout), &installed) != nil || len(installed.Versions) != len(spec.packages) || len(installed.Distributions) != len(spec.packages) || len(installed.Modules) != len(spec.packages) {
		return nil, fail(fmt.Errorf("invalid prepared package metadata"))
	}
	for _, pkg := range spec.packages {
		if installed.Versions[pkg.Name] != pkg.Version {
			return nil, fail(fmt.Errorf("prepared package %s does not match locked version", pkg.Name))
		}
		if !preparedWithin(rt.Venv, installed.Distributions[pkg.Name]) {
			return nil, fail(fmt.Errorf("prepared distribution %s is outside the dedicated venv", pkg.Name))
		}
	}
	for _, pkg := range spec.packages {
		if !preparedWithin(rt.Venv, installed.Modules[pkg.Name]) {
			return nil, fail(fmt.Errorf("prepared module %s is outside the dedicated venv", pkg.Name))
		}
	}
	if err = guardPreparedPaths(root, rt); err != nil {
		return nil, fail(err)
	}
	after, err := readPreparedRegular(markerPath, 8<<10)
	if err != nil || !bytes.Equal(marker, after) {
		return nil, fail(fmt.Errorf("preparation marker changed during validation"))
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if err = os.RemoveAll(scratch); err != nil {
		return nil, fail(fmt.Errorf("prepared scratch cleanup: %w", err))
	}
	var once sync.Once
	release = func() { once.Do(unlock) }
	success = true
	return release, nil
}

// ValidatePrepared is a point-in-time check. Use AcquirePrepared when an
// installation lease must remain held across a later execution.
func ValidatePrepared(ctx context.Context, rt Runtime) error {
	release, err := AcquirePrepared(ctx, rt)
	if err != nil {
		return err
	}
	release()
	return nil
}
func cleanAbsolute(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path
}
func samePreparedPath(a, b string) bool {
	if !cleanAbsolute(a) || !cleanAbsolute(b) {
		return false
	}
	a, ea := filepath.EvalSymlinks(a)
	b, eb := filepath.EvalSymlinks(b)
	return ea == nil && eb == nil && a == b
}
func guardPreparedPaths(root string, rt Runtime) error {
	if err := guardReal(root); err != nil {
		return err
	}
	realVenv := filepath.Join(root, "pyenv", "venv")
	if err := walkBelow(root, filepath.Dir(venvPython(realVenv))); err != nil {
		return err
	}
	for _, dir := range []string{root, rt.Venv, rt.Base.Prefix, rt.Base.Stdlib} {
		info, err := os.Stat(dir)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("runtime directory missing: %s", dir)
		}
	}
	return nil
}
func preparedExecutable(ctx context.Context, base, python string) error {
	a, err := os.Stat(base)
	if err != nil {
		return err
	}
	b, err := os.Stat(python)
	if err != nil {
		return err
	}
	if !a.Mode().IsRegular() || !b.Mode().IsRegular() || a.Mode().Perm()&0111 == 0 || b.Mode().Perm()&0111 == 0 {
		return fmt.Errorf("prepared interpreter is not an executable regular file")
	}
	if os.SameFile(a, b) {
		return nil
	}
	if a.Size() != b.Size() || a.Size() > 64<<20 {
		return fmt.Errorf("venv interpreter differs from its base")
	}
	hash := func(path string) ([32]byte, error) {
		var result [32]byte
		f, err := os.Open(path)
		if err != nil {
			return result, err
		}
		defer f.Close()
		h := sha256.New()
		buf := make([]byte, 32<<10)
		var total int64
		for {
			if err = ctx.Err(); err != nil {
				return result, err
			}
			n, e := f.Read(buf)
			total += int64(n)
			if total > 64<<20 {
				return result, fmt.Errorf("copied interpreter exceeds validation budget")
			}
			if n > 0 {
				_, _ = h.Write(buf[:n])
			}
			if e == io.EOF {
				break
			}
			if e != nil {
				return result, e
			}
		}
		copy(result[:], h.Sum(nil))
		return result, nil
	}
	first, err := hash(base)
	if err != nil {
		return err
	}
	second, err := hash(python)
	if err != nil {
		return err
	}
	if first != second {
		return fmt.Errorf("venv interpreter differs from its base")
	}
	return nil
}

type preparedInfo struct {
	Executable, BaseExecutable, Prefix, BasePrefix, Stdlib string
	Version                                                Version
}
type preparedProbe struct {
	Executable     string `json:"executable"`
	BaseExecutable string `json:"base_executable"`
	Prefix         string `json:"prefix"`
	BasePrefix     string `json:"base_prefix"`
	Stdlib         string `json:"stdlib"`
	Version        []int  `json:"version"`
}

const preparedMetadataSnippet = `import json,sys,sysconfig
print(json.dumps(dict(executable=sys.executable,base_executable=sys._base_executable,prefix=sys.prefix,base_prefix=sys.base_prefix,stdlib=sysconfig.get_path('stdlib'),version=list(sys.version_info[:3]))))`

type preparedPackages struct {
	Versions      map[string]string `json:"versions"`
	Distributions map[string]string `json:"distributions"`
	Modules       map[string]string `json:"modules"`
}

// Module names are frozen with the embedded 14-distribution lock. Package
// metadata alone can survive deletion of its importable module, so import every
// dependency and return its actual origin instead of trusting dist-info alone.
const preparedImportSnippet = `import importlib,importlib.metadata as metadata,json,sys
import pandas,matplotlib
module_names={
 'contourpy':'contourpy','cycler':'cycler','fonttools':'fontTools',
 'kiwisolver':'kiwisolver','matplotlib':'matplotlib','numpy':'numpy',
 'packaging':'packaging','pandas':'pandas','pillow':'PIL','pyparsing':'pyparsing',
 'python-dateutil':'dateutil','pytz':'pytz','six':'six','tzdata':'tzdata'
}
distributions={name:metadata.distribution(name) for name in sys.argv[1:]}
modules={name:importlib.import_module(module_names[name]) for name in sys.argv[1:]}
print(json.dumps(dict(versions={name:dist.version for name,dist in distributions.items()},distributions={name:str(dist.locate_file('')) for name,dist in distributions.items()},modules={name:module.__file__ for name,module in modules.items()})))`

func preparedMetadata(ctx context.Context, env []string, python string) (preparedInfo, error) {
	var info preparedInfo
	result, err := runCmdEnv(ctx, env, python, "-I", "-B", "-c", preparedMetadataSnippet)
	if err != nil {
		return info, fmt.Errorf("prepared metadata: %w (%s)", err, trimDetail(firstOutput(result)))
	}
	var probe preparedProbe
	if err = json.Unmarshal(bytes.TrimSpace(result.Stdout), &probe); err != nil || len(probe.Version) != 3 {
		return info, fmt.Errorf("invalid prepared interpreter metadata")
	}
	info = preparedInfo{probe.Executable, probe.BaseExecutable, probe.Prefix, probe.BasePrefix, probe.Stdlib, Version{probe.Version[0], probe.Version[1], probe.Version[2]}}
	return info, nil
}
func preparedEnv(scratch string) []string {
	env := []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "PYTHONDONTWRITEBYTECODE=1", "MPLBACKEND=Agg", "HOME=" + scratch, "TMPDIR=" + scratch, "XDG_CACHE_HOME=" + filepath.Join(scratch, "cache"), "MPLCONFIGDIR=" + scratch}
	for _, name := range strings.Fields("OPENBLAS_NUM_THREADS OMP_NUM_THREADS MKL_NUM_THREADS NUMEXPR_NUM_THREADS VECLIB_MAXIMUM_THREADS BLIS_NUM_THREADS") {
		env = append(env, name+"=1")
	}
	return env
}

// Provenance is a directory-boundary check, not a hash attestation of packages.
// Resolve symlinks before containment so a venv .pth/global fallback cannot
// supply a matching-version distribution or imported top-level module elsewhere.
func preparedWithin(venv, path string) bool {
	if !cleanAbsolute(path) {
		return false
	}
	root, err := filepath.EvalSymlinks(venv)
	if err != nil {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, resolved)
	return err == nil && !escapes(rel)
}
