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
// 锁文件和 venv 标记一致且解释器仍可用时不会再次联网。
// 这一步在沙箱外执行。失败且属于运行时问题时，错误码是 runtime_unavailable。
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
	want := hex.EncodeToString(sum[:])
	ready, err := venvReady(ctx, py, filepath.Join(venv, markerName), want)
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
	if err := writeFileAtomic(filepath.Join(venv, markerName), []byte(want+"\n"), 0o644); err != nil {
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

func venvReady(ctx context.Context, py, markerPath, want string) (bool, error) {
	body, err := os.ReadFile(markerPath)
	if err != nil {
		return false, nil
	}
	if strings.TrimSpace(string(body)) != want {
		return false, nil
	}
	if _, err := inspect(ctx, py); err != nil {
		if ctxErr(err) {
			return false, err
		}
		return false, nil
	}
	return true, nil
}

func createVenv(ctx context.Context, base, venv, py string) error {
	if st, err := os.Stat(py); err == nil && !st.IsDir() {
		if _, ierr := inspect(ctx, py); ierr == nil {
			return nil
		} else if ctxErr(ierr) {
			return ierr
		}
	}
	args := []string{"-I", "-m", "venv"}
	if st, err := os.Stat(venv); err == nil && st.IsDir() {
		args = append(args, "--clear")
	}
	args = append(args, venv)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	res, err := runCmd(ctx, base, args...)
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
	res, err := runCmd(ctx, py,
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
