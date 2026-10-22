//go:build linux && amd64

package sandbox

import (
	"context"
	"debug/elf"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type runtimePlan struct {
	Python    string
	Stdlib    string
	Venv      string
	ReadDirs  []string
	ReadFiles []string
	ExecFiles []string
	Warnings  []string
}

func canonical(path string) (string, error) {
	p, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}
func prepareRuntime(ctx context.Context, rt Runtime, extra []string) (runtimePlan, error) {
	var plan runtimePlan
	python := rt.Python
	if python == "" {
		var err error
		python, err = exec.LookPath("python3")
		if err != nil {
			return plan, RuntimeUnavailable("python3 missing")
		}
	}
	original, err := filepath.Abs(python)
	if err != nil {
		return plan, RuntimeUnavailable(err.Error())
	}
	plan.Python, err = canonical(original)
	if err != nil {
		return plan, RuntimeUnavailable(err.Error())
	}
	// This is a fixed metadata probe of the trusted configured interpreter, never user code.
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, original, "-I", "-B", "-c", `import json,sys,sysconfig; print(json.dumps(dict(stdlib=sysconfig.get_path('stdlib'),prefix=sys.prefix,base=sys.base_prefix,version=list(sys.version_info[:2]))))`)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "PYTHONDONTWRITEBYTECODE=1"}
	data, err := cmd.Output()
	if err != nil {
		return plan, RuntimeUnavailable("trusted Python metadata probe failed: " + err.Error())
	}
	var info struct {
		Stdlib, Prefix, Base string
		Version              []int
	}
	if json.Unmarshal(data, &info) != nil || len(info.Version) != 2 || info.Version[0] != 3 || info.Version[1] < 10 {
		return plan, RuntimeUnavailable("Python >=3.10 metadata required")
	}
	stdlib := rt.PyRoot
	if stdlib == "" {
		stdlib = info.Stdlib
	}
	plan.Stdlib, err = canonical(stdlib)
	if err != nil {
		return plan, RuntimeUnavailable("stdlib: " + err.Error())
	}
	if filepath.Base(plan.Stdlib) == "usr" || plan.Stdlib == "/" {
		return plan, RuntimeUnavailable("PyRoot must be stdlib, not a system prefix")
	}
	venv := rt.Venv
	if venv == "" && info.Prefix != info.Base {
		venv = info.Prefix
	}
	if venv != "" {
		plan.Venv, err = canonical(venv)
		if err != nil {
			return plan, RuntimeUnavailable("venv: " + err.Error())
		}
	}
	plan.ReadDirs = []string{plan.Stdlib}
	if plan.Venv != "" {
		plan.ReadDirs = append(plan.ReadDirs, plan.Venv)
	}
	for _, p := range extra {
		q, e := canonical(p)
		if e != nil {
			return plan, RuntimeUnavailable("approved read directory: " + e.Error())
		}
		fi, e := os.Stat(q)
		if e != nil || !fi.IsDir() {
			return plan, RuntimeUnavailable("approved path is not a directory")
		}
		plan.ReadDirs = append(plan.ReadDirs, q)
	}
	// Recursively parse ELF loader/DT_NEEDED. Never execute ldd or permit /usr wholesale.
	seen := map[string]bool{}
	files := map[string]bool{}
	var resolve func(string) error
	resolve = func(path string) error {
		real, e := canonical(path)
		if e != nil {
			return e
		}
		if seen[real] {
			return nil
		}
		seen[real] = true
		f, e := elf.Open(real)
		if e != nil {
			return fmt.Errorf("ELF %s: %w", real, e)
		}
		defer f.Close()
		files[real] = true
		for _, p := range f.Progs {
			if p.Type == elf.PT_INTERP {
				b := make([]byte, p.Filesz)
				if _, e = p.ReadAt(b, 0); e != nil {
					return e
				}
				loader := strings.TrimRight(string(b), "\x00")
				realLoader, e := canonical(loader)
				if e != nil {
					return e
				}
				plan.ExecFiles = append(plan.ExecFiles, realLoader)
				if e = resolve(loader); e != nil {
					return e
				}
			}
		}
		needed, e := f.ImportedLibraries()
		if e != nil {
			return e
		}
		var dirs []string
		for _, tag := range []elf.DynTag{elf.DT_RUNPATH, elf.DT_RPATH} {
			paths, _ := f.DynString(tag)
			for _, paths := range paths {
				for _, d := range strings.Split(paths, ":") {
					d = strings.ReplaceAll(strings.ReplaceAll(d, "${ORIGIN}", filepath.Dir(real)), "$ORIGIN", filepath.Dir(real))
					if filepath.IsAbs(d) {
						dirs = append(dirs, d)
					}
				}
			}
		}
		dirs = append(dirs, "/lib/x86_64-linux-gnu", "/usr/lib/x86_64-linux-gnu", "/lib64", "/usr/lib64", "/lib", "/usr/lib", "/usr/local/lib")
		for _, lib := range needed {
			found := ""
			for _, dir := range dirs {
				p := filepath.Join(dir, lib)
				if fi, e := os.Stat(p); e == nil && fi.Mode().IsRegular() {
					found = p
					break
				}
			}
			if found == "" {
				return fmt.Errorf("missing ELF dependency %s of %s", lib, real)
			}
			if e = resolve(found); e != nil {
				return e
			}
		}
		return nil
	}
	if err = resolve(plan.Python); err != nil {
		return plan, RuntimeUnavailable(err.Error())
	}
	for _, dir := range []string{plan.Stdlib, plan.Venv} {
		if dir == "" {
			continue
		}
		err = filepath.WalkDir(dir, func(path string, d os.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !d.IsDir() && (strings.HasSuffix(d.Name(), ".so") || strings.Contains(d.Name(), ".so.")) {
				if e := resolve(path); e != nil { // Optional host extensions may themselves be unusable.
					// Leave missing dependencies inaccessible; never widen the allowlist.
					if strings.Contains(e.Error(), "missing ELF dependency") {
						plan.Warnings = append(plan.Warnings, e.Error())
						return nil
					}
					return e
				}
				return nil
			}
			return nil
		})
		if err != nil {
			return plan, RuntimeUnavailable(err.Error())
		}
	}
	// venv Python startup reads pyvenv.cfg adjacent to its original executable.
	if plan.Venv != "" {
		p := filepath.Join(plan.Venv, "pyvenv.cfg")
		if _, err = os.Stat(p); err != nil {
			return plan, RuntimeUnavailable(err.Error())
		}
		files[p] = true
		plan.Python = original
	}
	for p := range files {
		plan.ReadFiles = append(plan.ReadFiles, p)
	}
	sort.Strings(plan.ReadFiles)
	return plan, nil
}
