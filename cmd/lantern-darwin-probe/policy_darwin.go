//go:build darwin

package main

import (
	"debug/macho"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/loveRyujin/lantern/core/pyrt"
)

type readPlan struct {
	Directories     []string `json:"directories"`
	OpenDirectories []string `json:"open_directories"`
	Files           []string `json:"files"`
	Executables     []string `json:"executables"`
	Unresolved      []string `json:"optional_unresolved"`
}

func canonical(path string) string {
	p, e := filepath.EvalSymlinks(path)
	if e == nil {
		return p
	}
	return filepath.Clean(path)
}
func listSorted(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for s := range m {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Static Mach-O inspection only. System shared-cache libraries need not exist
// as individual files; their literal install names are retained. Optional
// extension failures never cause a whole-prefix read grant.
func runtimeReadPlan(rt pyrt.Runtime) (readPlan, error) {
	dirs := map[string]bool{canonical(rt.Venv): true, canonical(rt.Base.Stdlib): true}
	openDirs := map[string]bool{}
	// Native fixed single-directory trials established that adding literal /
	// lets the payload enter, while the ten other single additions did not.
	// fs_usage also records opening the Cryptex root. These open/list grants
	// cover only the exact directories, never child contents. The provisional
	// /usr/lib grant is removed; the complete native suite must revalidate this
	// deliberately selected baseline, not automatically fall back to a trial.
	for _, p := range []string{"/", "/System/Volumes/Preboot/Cryptexes/OS"} {
		if st, e := os.Stat(p); e == nil && st.IsDir() {
			openDirs[p] = true
			openDirs[canonical(p)] = true
		}
	}
	files := map[string]bool{rt.Python: true, canonical(rt.Python): true, rt.Base.Path: true, canonical(rt.Base.Path): true,
		"/dev/null": true, "/dev/urandom": true, "/dev/random": true, "/etc/localtime": true, "/private/etc/localtime": true}
	for _, p := range []string{"/System/Library/dyld", "/System/Volumes/Preboot/Cryptexes/OS/System/Library/dyld"} {
		if st, e := os.Stat(p); e == nil && st.IsDir() {
			dirs[p] = true
			dirs[canonical(p)] = true // same already-stat'ed cache vnode, not a wider read root
		}
	}
	warnings := map[string]bool{}
	seen := map[string]bool{}
	var visit func(string, bool) error
	visit = func(path string, required bool) error {
		path = canonical(path)
		if seen[path] {
			return nil
		}
		seen[path] = true
		f, e := macho.Open(path)
		if e != nil {
			fat, fe := macho.OpenFat(path)
			if fe == nil {
				defer fat.Close()
				wanted := macho.CpuArm64
				if runtime.GOARCH == "amd64" {
					wanted = macho.CpuAmd64
				}
				for _, a := range fat.Arches {
					if a.Cpu == wanted {
						f = a.File
						e = nil
						break
					}
				}
			}
		}
		if e != nil {
			if required {
				return fmt.Errorf("Mach-O %s: %w", path, e)
			}
			return nil
		}
		defer f.Close()
		files[path] = true
		libs, e := f.ImportedLibraries()
		if e != nil {
			return e
		}
		rpaths := []string{filepath.Join(rt.Base.Prefix, "lib"), filepath.Dir(canonical(rt.Python))}
		for _, load := range f.Loads {
			b := load.Raw()
			if len(b) >= 12 && f.ByteOrder.Uint32(b[:4]) == 0x8000001c {
				offset := int(f.ByteOrder.Uint32(b[8:12]))
				if offset >= 12 && offset < len(b) {
					rpaths = append(rpaths, strings.TrimRight(string(b[offset:]), "\x00"))
				}
			}
		}
		expand := func(p string) string {
			p = strings.ReplaceAll(p, "@loader_path", filepath.Dir(path))
			return strings.ReplaceAll(p, "@executable_path", filepath.Dir(canonical(rt.Python)))
		}
		for _, lib := range libs {
			candidate := expand(lib)
			if strings.HasPrefix(lib, "@rpath/") {
				candidate = ""
				for _, rp := range rpaths {
					p := filepath.Join(expand(rp), strings.TrimPrefix(lib, "@rpath/"))
					if _, e := os.Stat(p); e == nil {
						candidate = p
						break
					}
				}
			}
			if candidate == "" || !filepath.IsAbs(candidate) {
				warnings[path+": "+lib] = true
				continue
			}
			files[candidate] = true
			files[canonical(candidate)] = true
			if _, e := os.Stat(candidate); e != nil {
				if strings.HasPrefix(candidate, "/usr/lib/") || strings.HasPrefix(candidate, "/System/Library/") {
					continue
				}
				warnings[path+": "+candidate] = true
				continue
			}
			if e := visit(candidate, false); e != nil {
				warnings[e.Error()] = true
			}
		}
		return nil
	}
	if e := visit(rt.Python, true); e != nil {
		return readPlan{}, e
	}
	executables := map[string]bool{rt.Python: true, canonical(rt.Python): true}
	// CPython's official framework bin/python launcher uses POSIX_SPAWN_SETEXEC
	// to enter this exact executable. It is not a dylib dependency and must be
	// included separately; no whole-framework or general exec grant is needed.
	frameworkPython := filepath.Join(rt.Base.Prefix, "Resources", "Python.app", "Contents", "MacOS", "Python")
	if info, e := os.Stat(frameworkPython); e == nil && info.Mode().IsRegular() {
		if e = visit(frameworkPython, true); e != nil {
			return readPlan{}, e
		}
		executables[frameworkPython] = true
		executables[canonical(frameworkPython)] = true
	}
	for _, root := range []string{rt.Base.Stdlib, rt.Venv} {
		if e := filepath.WalkDir(root, func(path string, d os.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if d.IsDir() {
				return nil
			}
			if strings.HasSuffix(path, ".so") || strings.HasSuffix(path, ".dylib") {
				return visit(path, false)
			}
			return nil
		}); e != nil {
			return readPlan{}, e
		}
	}
	return readPlan{Directories: listSorted(dirs), OpenDirectories: listSorted(openDirs), Files: listSorted(files), Executables: listSorted(executables), Unresolved: listSorted(warnings)}, nil
}

func makePolicy(plan readPlan, c taskConfig, exe string) string {
	var b strings.Builder
	b.WriteString("(version 1)\n(deny default)\n")
	// Metadata is deliberately recorded as a residual disclosure. Content reads
	// stay constrained to the explicit runtime/task roots and exact dependencies.
	b.WriteString("(allow file-read-metadata)\n(allow sysctl-read)\n(allow process-info* (target self))\n(allow signal (target self))\n")
	b.WriteString("(deny network*)\n(deny process-fork)\n")
	for _, p := range plan.OpenDirectories {
		fmt.Fprintf(&b, "(allow file-read-data (literal %s))\n", strconv.Quote(p))
	}
	for _, p := range plan.Directories {
		fmt.Fprintf(&b, "(allow file-read* file-map-executable (subpath %s))\n", strconv.Quote(p))
	}
	for _, p := range plan.Files {
		fmt.Fprintf(&b, "(allow file-read* file-map-executable (literal %s))\n", strconv.Quote(p))
	}
	for _, p := range []string{c.Python, canonical(c.Python), exe, canonical(exe), "/usr/bin/true"} {
		fmt.Fprintf(&b, "(allow process-exec (literal %s))\n", strconv.Quote(p))
	}
	for _, p := range plan.Executables {
		fmt.Fprintf(&b, "(allow process-exec (literal %s))\n", strconv.Quote(p))
	}
	for _, p := range []string{exe, canonical(exe), c.Script} {
		fmt.Fprintf(&b, "(allow file-read* file-map-executable (literal %s))\n", strconv.Quote(p))
	}
	if c.UnixSocket != "" {
		fmt.Fprintf(&b, "(allow file-read* file-write* (literal %s))\n", strconv.Quote(c.UnixSocket))
	}
	for _, p := range []string{filepath.Join(c.Root, "input")} {
		fmt.Fprintf(&b, "(allow file-read* (subpath %s))\n", strconv.Quote(p))
	}
	for _, p := range []string{c.Out, c.Tmp} {
		fmt.Fprintf(&b, "(allow file-read* file-write* (subpath %s))\n", strconv.Quote(p))
	}
	return b.String()
}
