package pyrt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const markerName = ".lantern-requirements.sha256"

// Runtime 是已经准备好的专用 venv。
// Python 是 venv 里的解释器。Base 是创建它的本机解释器。
// 交给沙箱时：Python → sandbox.Runtime.Python，Base.Prefix → PyRoot，Venv → Venv。
type Runtime struct {
	Base   Interpreter
	Venv   string
	Python string
}

// Ensure 探测解释器，在 <Root>/pyenv/venv 创建 venv，并安装哈希锁定的依赖。
// 标记里的锁哈希、基础解释器路径和版本都一致，且 venv 解释器仍在锁定范围内时，不再联网。
// 标记缺失或不一致时，在 Root 里的临时目录生成新 venv，再原子 rename 替换旧目录。不对已有目录使用 --clear。
// Root、Root/pyenv 或路径上任一层是符号链接时返回 path_escape，不创建也不删除。
// 写标记前会在 venv 里 import pandas 和 matplotlib。
// 这一步在沙箱外执行。同一个 Root 上不要并发调用；本函数不加文件锁。
// 失败且属于运行时问题时，错误码是 runtime_unavailable。路径逃逸不是这个码。
func (c Config) Ensure(ctx context.Context) (Runtime, error) {
	if err := ctx.Err(); err != nil {
		return Runtime{}, err
	}
	if strings.TrimSpace(c.Root) == "" {
		return Runtime{}, errors.New("pyrt: Root 为空")
	}
	root, err := filepath.Abs(c.Root)
	if err != nil {
		return Runtime{}, fmt.Errorf("pyrt: Root: %w", err)
	}
	if err := guardWorkspace(root); err != nil {
		return Runtime{}, err
	}
	if testAfterPathCheck != nil {
		testAfterPathCheck()
	}
	if err := guardWorkspace(root); err != nil {
		return Runtime{}, err
	}
	base, err := c.Detect(ctx)
	if err != nil {
		return Runtime{}, err
	}
	spec, err := embeddedLock()
	if err != nil {
		return Runtime{}, fmt.Errorf("%w: %s", ErrLockfile, err.Error())
	}
	if !spec.supports(base.Version) {
		return Runtime{}, unavailable("lock", base.Path, "版本 "+base.Version.String()+" 不在锁定范围 "+spec.rangeString()+" 内")
	}
	pyenv := filepath.Join(root, "pyenv")
	if err := ensureRealTree(pyenv); err != nil {
		return Runtime{}, preferCoded("venv", pyenv, err)
	}
	if err := guardWorkspace(root); err != nil {
		return Runtime{}, err
	}
	venv := filepath.Join(pyenv, "venv")
	py := venvPython(venv)
	sum := sha256.Sum256(requirementsLock)
	lockHash := hex.EncodeToString(sum[:])
	ready, err := venvReady(ctx, py, filepath.Join(venv, markerName), lockHash, base, spec)
	if err != nil {
		return Runtime{}, err
	}
	if ready {
		return Runtime{Base: base, Venv: venv, Python: py}, nil
	}
	venv, py, err = installVenv(ctx, root, base.Path, pyenv)
	if err != nil {
		return Runtime{}, err
	}
	reqPath := filepath.Join(pyenv, "requirements.txt")
	if err := writeFileAtomic(reqPath, requirementsLock, 0o644); err != nil {
		return Runtime{}, preferCoded("install", reqPath, err)
	}
	if err := pipInstall(ctx, py, reqPath); err != nil {
		return Runtime{}, err
	}
	if err := verifyImports(ctx, py); err != nil {
		return Runtime{}, err
	}
	if err := writeFileAtomic(filepath.Join(venv, markerName), formatMarker(lockHash, base), 0o644); err != nil {
		return Runtime{}, preferCoded("install", venv, err)
	}
	return Runtime{Base: base, Venv: venv, Python: py}, nil
}

func venvPython(venv string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(venv, "Scripts", "python.exe")
	}
	return filepath.Join(venv, "bin", "python")
}

func venvReady(ctx context.Context, py, markerPath, lockHash string, base Interpreter, spec lockSpec) (bool, error) {
	body, err := os.ReadFile(markerPath)
	if err != nil {
		return false, nil
	}
	gotLock, gotBase, gotVersion, ok := parseMarker(string(body))
	if !ok || gotLock != lockHash || gotBase != base.Path || gotVersion != base.Version.String() {
		return false, nil
	}
	it, err := inspect(ctx, py)
	if err != nil {
		if ctxErr(err) {
			return false, err
		}
		return false, nil
	}
	if it.Version != base.Version || !spec.supports(it.Version) {
		return false, nil
	}
	return true, nil
}

func formatMarker(lockHash string, base Interpreter) []byte {
	return []byte(fmt.Sprintf("lock %s\nbase %s\nversion %s\n", lockHash, base.Path, base.Version.String()))
}

func parseMarker(text string) (lockHash, base, version string, ok bool) {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) != 3 {
		return "", "", "", false
	}
	lockHash, ok1 := strings.CutPrefix(strings.TrimSpace(lines[0]), "lock ")
	base, ok2 := strings.CutPrefix(strings.TrimSpace(lines[1]), "base ")
	version, ok3 := strings.CutPrefix(strings.TrimSpace(lines[2]), "version ")
	if !ok1 || !ok2 || !ok3 || lockHash == "" || base == "" || version == "" {
		return "", "", "", false
	}
	return lockHash, base, version, true
}

// installVenv 在 pyenv 里建一个空的临时目录，对其执行不带 --clear 的 venv，再 rename 成 venv。
// 旧目录先改名挪开。删除前再次确认整条路径都不是符号链接，并且仍在 Root 内。
func installVenv(ctx context.Context, root, base, pyenv string) (venv, py string, err error) {
	if err := guardWorkspace(root); err != nil {
		return "", "", err
	}
	tmp, err := os.MkdirTemp(pyenv, ".venv-new-")
	if err != nil {
		return "", "", preferCoded("venv", pyenv, err)
	}
	discard := true
	defer func() {
		if !discard {
			return
		}
		// 临时目录若已落到 Root 外，removeRealDir 会拒绝 RemoveAll。
		if rmErr := removeRealDir(root, tmp); rmErr != nil && err == nil {
			err = rmErr
		}
	}()
	if err = resolvedInside(root, tmp); err != nil {
		return "", "", err
	}
	py = venvPython(tmp)
	if err = createVenv(ctx, base, tmp, py); err != nil {
		return "", "", err
	}
	venv = filepath.Join(pyenv, "venv")
	info, statErr := os.Lstat(venv)
	bak := ""
	switch {
	case os.IsNotExist(statErr):
	case statErr != nil:
		return "", "", statErr
	default:
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", "", pathEscape(venv, "已有 venv 不是普通目录")
		}
		bak = filepath.Join(pyenv, fmt.Sprintf(".venv-old-%d", time.Now().UnixNano()))
		if err = os.Rename(venv, bak); err != nil {
			return "", "", preferCoded("venv", venv, err)
		}
	}
	if err = os.Rename(tmp, venv); err != nil {
		if bak != "" {
			_ = os.Rename(bak, venv)
		}
		return "", "", preferCoded("venv", venv, err)
	}
	discard = false
	if bak != "" {
		if err = removeRealDir(root, bak); err != nil {
			return "", "", err
		}
	}
	return venv, venvPython(venv), nil
}

// createVenv 只在空的临时目录上创建环境，不传 --clear。
// --clear 会在沙箱外删除目标目录；目标若是指向 Root 外的符号链接，删的就是外面的文件。
func createVenv(ctx context.Context, base, venv, py string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	res, err := runCmd(ctx, base, "-I", "-m", "venv", venv)
	if err != nil {
		if ctxErr(err) {
			return err
		}
		return unavailable("venv", venv, trimDetail(firstOutput(res)))
	}
	if _, err := inspect(ctx, py); err != nil {
		if ctxErr(err) {
			return err
		}
		return unavailable("venv", py, "venv 已创建但解释器不可用")
	}
	return nil
}

func pipInstall(ctx context.Context, py, requirements string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	// 清掉会把包装出 venv 的变量。index-url 等镜像配置留在环境里，pip 自己读。
	env := withoutEnvKeys(os.Environ(), "PIP_TARGET", "PIP_PREFIX", "PIP_USER")
	res, err := runCmdEnv(ctx, env, py,
		"-I", "-m", "pip", "install",
		"--disable-pip-version-check",
		"--no-input",
		"--require-hashes",
		"--only-binary=:all:",
		"-r", requirements,
	)
	if err != nil {
		if ctxErr(err) {
			return err
		}
		return unavailable("install", py, trimDetail(firstOutput(res)))
	}
	return nil
}

const importCheck = "import pandas, matplotlib"

func verifyImports(ctx context.Context, py string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	env := withoutEnvKeys(os.Environ(), "PIP_TARGET", "PIP_PREFIX", "PIP_USER", "MPLBACKEND")
	env = append(env, "MPLBACKEND=Agg")
	res, err := runCmdEnv(ctx, env, py, "-I", "-c", importCheck)
	if err != nil {
		if ctxErr(err) {
			return err
		}
		return unavailable("import", py, trimDetail(firstOutput(res)))
	}
	return nil
}

func withoutEnvKeys(env []string, keys ...string) []string {
	drop := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		drop[key] = struct{}{}
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if _, skip := drop[key]; skip {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return pathEscape(dir, "父目录是符号链接")
	}
	if !info.IsDir() {
		return fmt.Errorf("pyrt: %s 不是目录", dir)
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
