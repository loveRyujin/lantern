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
// 标记缺失或不一致时用 python -m venv --clear 重建。写标记前会在 venv 里 import pandas 和 matplotlib。
// 这一步在沙箱外执行。同一个 Root 上不要并发调用；本函数不加文件锁。
// 失败且属于运行时问题时，错误码是 runtime_unavailable。
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
	if err := os.MkdirAll(pyenv, 0o755); err != nil {
		return Runtime{}, unavailable("venv", pyenv, err.Error())
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
	if err := createVenv(ctx, base.Path, venv, py); err != nil {
		return Runtime{}, err
	}
	reqPath := filepath.Join(pyenv, "requirements.txt")
	if err := writeFileAtomic(reqPath, requirementsLock, 0o644); err != nil {
		return Runtime{}, unavailable("install", reqPath, err.Error())
	}
	if err := pipInstall(ctx, py, reqPath); err != nil {
		return Runtime{}, err
	}
	if err := verifyImports(ctx, py); err != nil {
		return Runtime{}, err
	}
	if err := writeFileAtomic(filepath.Join(venv, markerName), formatMarker(lockHash, base), 0o644); err != nil {
		return Runtime{}, unavailable("install", venv, err.Error())
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

// createVenv 总是带 --clear。标记缺失时目录里可能有装了一半的包，留下会污染下一次安装。
// 目标目录还不存在时，Python 的 --clear 也会直接建一个新环境。
func createVenv(ctx context.Context, base, venv, py string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	res, err := runCmd(ctx, base, "-I", "-m", "venv", "--clear", venv)
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
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
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
