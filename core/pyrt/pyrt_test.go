package pyrt_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/loveRyujin/lantern/core/errcode"
	"github.com/loveRyujin/lantern/core/pyrt"
)

func TestDetectUsesExplicitInterpreter(t *testing.T) {
	explicit := installFake(t, t.TempDir(), "python3", "3.11.2")
	otherDir := t.TempDir()
	installFake(t, otherDir, "python3", "3.12.1")
	t.Setenv("PATH", otherDir)
	t.Setenv("PYRT_FAKE_LOG", filepath.Join(t.TempDir(), "log"))

	got, err := pyrt.Config{
		Python:      explicit,
		SearchPaths: []string{},
	}.Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Version.String() != "3.11.2" {
		t.Fatalf("版本 = %s", got.Version)
	}
	if got.Path != explicit {
		t.Fatalf("路径 = %s，想要 %s", got.Path, explicit)
	}
	if !strings.HasPrefix(got.Prefix, "/") || !strings.HasPrefix(got.Stdlib, got.Prefix) {
		t.Fatalf("Prefix=%s Stdlib=%s", got.Prefix, got.Stdlib)
	}
}

func TestDetectExplicitTooOldDoesNotFallBack(t *testing.T) {
	explicit := installFake(t, t.TempDir(), "python3", "3.9.18")
	pathDir := t.TempDir()
	installFake(t, pathDir, "python3", "3.12.1")
	t.Setenv("PATH", pathDir)

	_, err := pyrt.Config{Python: explicit, SearchPaths: []string{}}.Detect(context.Background())
	if !errors.Is(err, pyrt.ErrUnavailable) {
		t.Fatalf("err=%v", err)
	}
	code, ok := errcode.CodeOf(err)
	if !ok || code != errcode.RuntimeUnavailable {
		t.Fatalf("CodeOf=%s ok=%v", code, ok)
	}
	if !strings.Contains(err.Error(), "3.9.18") || !strings.Contains(err.Error(), "低于 3.10") {
		t.Fatalf("err=%v", err)
	}
}

func TestDetectFallsBackToSearchPath(t *testing.T) {
	pathDir := t.TempDir()
	installFake(t, pathDir, "python3", "3.9.1")
	newer := installFake(t, t.TempDir(), "python3", "3.12.4")
	t.Setenv("PATH", pathDir)

	got, err := pyrt.Config{SearchPaths: []string{newer}}.Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Version.String() != "3.12.4" || got.Path != newer {
		t.Fatalf("got=%+v", got)
	}
}

func TestDetectMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := pyrt.Config{SearchPaths: []string{}}.Detect(context.Background())
	assertUnavailable(t, err)
	if !strings.Contains(err.Error(), "没有找到 python3") {
		t.Fatalf("err=%v", err)
	}
}

func TestDetectBrokenInterpreter(t *testing.T) {
	dir := t.TempDir()
	path := installFake(t, dir, "python3", "3.12.1")
	t.Setenv("PATH", dir)
	t.Setenv("PYRT_FAKE_GARBAGE", "1")
	_, err := pyrt.Config{SearchPaths: []string{}}.Detect(context.Background())
	assertUnavailable(t, err)
	if !strings.Contains(err.Error(), "无法解析版本输出") {
		t.Fatalf("err=%v", err)
	}

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PYRT_FAKE_GARBAGE", "")
	_, err = pyrt.Config{Python: path, SearchPaths: []string{}}.Detect(context.Background())
	assertUnavailable(t, err)

	_, err = pyrt.Config{Python: dir, SearchPaths: []string{}}.Detect(context.Background())
	assertUnavailable(t, err)

	missing := filepath.Join(t.TempDir(), "missing")
	_, err = pyrt.Config{Python: missing, SearchPaths: []string{}}.Detect(context.Background())
	assertUnavailable(t, err)
}

func TestDetectCanceled(t *testing.T) {
	dir := t.TempDir()
	installFake(t, dir, "python3", "3.12.1")
	t.Setenv("PATH", dir)
	t.Setenv("PYRT_FAKE_SLEEP", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := pyrt.Config{SearchPaths: []string{}}.Detect(ctx)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("取消后等了 %s", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	if errors.Is(err, pyrt.ErrUnavailable) {
		t.Fatal("取消不应该报 runtime_unavailable")
	}
}

func TestEnsureInstallsHashLockedRequirements(t *testing.T) {
	dir := t.TempDir()
	installFake(t, dir, "python3", "3.12.1")
	logPath := filepath.Join(t.TempDir(), "log")
	t.Setenv("PATH", dir)
	t.Setenv("PYRT_FAKE_LOG", logPath)
	root := t.TempDir()

	rt, err := pyrt.Config{Root: root, SearchPaths: []string{}}.Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rt.Python != filepath.Join(root, "pyenv", "venv", "bin", "python") {
		t.Fatalf("Python=%s", rt.Python)
	}
	if rt.Venv != filepath.Join(root, "pyenv", "venv") {
		t.Fatalf("Venv=%s", rt.Venv)
	}
	if rt.Base.Version.String() != "3.12.1" {
		t.Fatalf("base=%s", rt.Base.Version)
	}
	body, err := os.ReadFile(filepath.Join(root, "pyenv", "requirements.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "pandas==2.3.3") || !strings.Contains(string(body), "matplotlib==3.10.9") || !strings.Contains(string(body), "--hash=sha256:") {
		t.Fatal("写出的 requirements 不是哈希锁定文件")
	}
	log := readLog(t, logPath)
	if strings.Count(log, " -m pip ") != 1 || !strings.Contains(log, "--require-hashes") || !strings.Contains(log, "--only-binary=:all:") {
		t.Fatalf("pip 调用不符合预期:\n%s", log)
	}

	if _, err := (pyrt.Config{Root: root, SearchPaths: []string{}}).Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	log = readLog(t, logPath)
	if strings.Count(log, " -m pip ") != 1 {
		t.Fatalf("第二次 Ensure 又安装了依赖:\n%s", log)
	}
}

func TestEnsureUnavailable(t *testing.T) {
	_, err := pyrt.Config{}.Ensure(context.Background())
	if err == nil || errors.Is(err, pyrt.ErrUnavailable) {
		t.Fatalf("空 Root err=%v", err)
	}
	if _, ok := errcode.CodeOf(err); ok {
		t.Fatal("空 Root 不应该带错误码")
	}

	dir := t.TempDir()
	installFake(t, dir, "python3", "3.8.0")
	t.Setenv("PATH", dir)
	fresh := t.TempDir()
	_, err = pyrt.Config{Root: fresh, SearchPaths: []string{}}.Ensure(context.Background())
	assertUnavailable(t, err)
	if _, statErr := os.Stat(filepath.Join(fresh, "pyenv")); !os.IsNotExist(statErr) {
		t.Fatal("版本太低时不应该创建 pyenv")
	}

	newDir := t.TempDir()
	installFake(t, newDir, "python3", "3.14.0")
	t.Setenv("PATH", newDir)
	outside := t.TempDir()
	_, err = pyrt.Config{Root: outside, SearchPaths: []string{}}.Ensure(context.Background())
	assertUnavailable(t, err)
	if !strings.Contains(err.Error(), "3.14.0") || !strings.Contains(err.Error(), "<3.14") {
		t.Fatalf("err=%v", err)
	}
	if _, statErr := os.Stat(filepath.Join(outside, "pyenv")); !os.IsNotExist(statErr) {
		t.Fatal("超出锁定范围时不应该创建 pyenv")
	}
}

func TestEnsurePipFailure(t *testing.T) {
	dir := t.TempDir()
	installFake(t, dir, "python3", "3.12.1")
	t.Setenv("PATH", dir)
	t.Setenv("PYRT_FAKE_PIP_FAIL", "1")
	root := t.TempDir()
	_, err := pyrt.Config{Root: root, SearchPaths: []string{}}.Ensure(context.Background())
	assertUnavailable(t, err)
	if !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("err=%v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "pyenv", "venv", ".lantern-requirements.sha256")); !os.IsNotExist(statErr) {
		t.Fatal("安装失败时不应该写下成功标记")
	}
}

func TestEnsureReal(t *testing.T) {
	if os.Getenv("PYRT_ENSURE_REAL") != "1" {
		t.Skip("设置 PYRT_ENSURE_REAL=1 才会创建 venv 并安装锁定依赖")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	rt, err := pyrt.Config{Root: root, Python: python, SearchPaths: []string{}}.Ensure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, rt.Python, "-I", "-c", "import pandas, matplotlib; print(pandas.__version__); print(matplotlib.__version__)")
	cmd.Env = append(os.Environ(), "MPLBACKEND=Agg")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("import: %v\n%s", err, out)
	}
	text := string(out)
	if !strings.Contains(text, "2.3.3") || !strings.Contains(text, "3.10.9") {
		t.Fatalf("版本输出:\n%s", text)
	}
	if _, err := (pyrt.Config{Root: root, Python: python, SearchPaths: []string{}}).Ensure(ctx); err != nil {
		t.Fatal(err)
	}
}

func installFake(t *testing.T, dir, name, version string) string {
	t.Helper()
	body, err := os.ReadFile("testdata/fake-python.sh")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, body, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".ver", []byte(version+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertUnavailable(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, pyrt.ErrUnavailable) {
		t.Fatalf("err=%v，想要 runtime_unavailable", err)
	}
	code, ok := errcode.CodeOf(err)
	if !ok || code != errcode.RuntimeUnavailable || code.String() != "runtime_unavailable" {
		t.Fatalf("CodeOf=%s ok=%v", code, ok)
	}
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
