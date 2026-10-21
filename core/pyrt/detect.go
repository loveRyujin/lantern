package pyrt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Interpreter 是一次探测得到的本机解释器。
// Prefix 是 sys.base_prefix，Stdlib 是标准库目录。沙箱要只读挂载整棵 Prefix，
// 只挂 Stdlib 会漏掉解释器本身和 lib-dynload。
type Interpreter struct {
	Path    string
	Version Version
	Prefix  string
	Stdlib  string
}

// Config 是探测和安装的输入。
type Config struct {
	// Root 是应用数据目录。Ensure 把 venv 建在 <Root>/pyenv/venv。
	Root string
	// Python 是设置里指定的解释器。非空时只使用它，版本不符也不改试别的路径。
	Python string
	// SearchPaths 是 PATH 之后继续尝试的绝对路径。
	// nil 表示 DefaultSearchPaths。空切片表示只查 Python 和 PATH。
	SearchPaths []string
}

// DefaultSearchPaths 返回当前系统在 PATH 之后要尝试的常见解释器位置。
func DefaultSearchPaths() []string {
	return searchPaths(runtime.GOOS)
}

func searchPaths(goos string) []string {
	switch goos {
	case "darwin":
		return []string{
			"/opt/homebrew/bin/python3",
			"/usr/local/bin/python3",
			"/usr/bin/python3",
		}
	case "linux":
		return []string{
			"/usr/local/bin/python3",
			"/usr/bin/python3",
		}
	default:
		return nil
	}
}

const probeSnippet = `import json,sys,sysconfig
json.dump({"major":sys.version_info[0],"minor":sys.version_info[1],"micro":sys.version_info[2],"executable":sys.executable,"base_prefix":sys.base_prefix,"stdlib":sysconfig.get_path("stdlib")},sys.stdout)
`

type probeInfo struct {
	Major      int    `json:"major"`
	Minor      int    `json:"minor"`
	Micro      int    `json:"micro"`
	Executable string `json:"executable"`
	BasePrefix string `json:"base_prefix"`
	Stdlib     string `json:"stdlib"`
}

// Detect 寻找版本不低于 3.10 的解释器。
// 找不到或版本太低时返回 ErrUnavailable，码值是 runtime_unavailable。
// ctx 已取消时返回 ctx 的错误，不把它说成运行时不可用。
func (c Config) Detect(ctx context.Context) (Interpreter, error) {
	if err := ctx.Err(); err != nil {
		return Interpreter{}, err
	}
	cands, err := c.candidates()
	if err != nil {
		return Interpreter{}, err
	}
	if len(cands) == 0 {
		return Interpreter{}, unavailable("detect", "", "没有找到 python3")
	}
	var last error
	var tooOld error
	tried := make([]string, 0, len(cands))
	for _, path := range cands {
		tried = append(tried, path)
		it, ierr := inspect(ctx, path)
		if ierr == nil {
			return it, nil
		}
		if !errors.Is(ierr, ErrUnavailable) {
			return Interpreter{}, ierr
		}
		last = ierr
		var uerr *UnavailableError
		if errors.As(ierr, &uerr) && uerr.Op == "version" {
			tooOld = ierr
		}
		if c.Python != "" {
			return Interpreter{}, ierr
		}
	}
	if tooOld != nil {
		return Interpreter{}, tooOld
	}
	if last != nil && len(cands) == 1 {
		return Interpreter{}, last
	}
	return Interpreter{}, unavailable("detect", "", "没有找到可用的 Python ≥3.10，已尝试: "+strings.Join(tried, ", "))
}

func (c Config) candidates() ([]string, error) {
	if strings.TrimSpace(c.Python) != "" {
		abs, err := filepath.Abs(c.Python)
		if err != nil {
			return nil, unavailable("detect", c.Python, "无法解析解释器路径")
		}
		return []string{abs}, nil
	}
	var out []string
	if p, err := exec.LookPath("python3"); err == nil {
		if abs, aerr := filepath.Abs(p); aerr == nil {
			p = abs
		}
		out = append(out, p)
	}
	search := c.SearchPaths
	if search == nil {
		search = DefaultSearchPaths()
	}
	for _, p := range search {
		if strings.TrimSpace(p) == "" {
			continue
		}
		if abs, aerr := filepath.Abs(p); aerr == nil {
			p = abs
		}
		out = append(out, p)
	}
	return dedup(out), nil
}

func dedup(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if strings.TrimSpace(p) == "" {
			continue
		}
		p = filepath.Clean(p)
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

func inspect(ctx context.Context, path string) (Interpreter, error) {
	if err := ctx.Err(); err != nil {
		return Interpreter{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return Interpreter{}, unavailable("detect", path, "解释器不存在")
	}
	if info.IsDir() {
		return Interpreter{}, unavailable("detect", path, "解释器路径是目录")
	}
	if runtime.GOOS != "windows" && info.Mode()&0o111 == 0 {
		return Interpreter{}, unavailable("detect", path, "解释器没有执行权限")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	res, err := runCmd(ctx, path, "-I", "-c", probeSnippet)
	if err != nil {
		if ctxErr(err) {
			return Interpreter{}, err
		}
		return Interpreter{}, unavailable("detect", path, trimDetail(firstOutput(res)))
	}
	var got probeInfo
	if err := json.Unmarshal(bytes.TrimSpace(res.Stdout), &got); err != nil {
		return Interpreter{}, unavailable("detect", path, "无法解析版本输出")
	}
	ver := Version{Major: got.Major, Minor: got.Minor, Micro: got.Micro}
	if !ver.AtLeast(3, 10) {
		return Interpreter{}, unavailable("version", path, "版本 "+ver.String()+" 低于 3.10")
	}
	if !filepath.IsAbs(got.Executable) || !filepath.IsAbs(got.BasePrefix) || !filepath.IsAbs(got.Stdlib) {
		return Interpreter{}, unavailable("detect", path, "解释器或标准库路径不是绝对路径")
	}
	return Interpreter{
		Path:    got.Executable,
		Version: ver,
		Prefix:  got.BasePrefix,
		Stdlib:  got.Stdlib,
	}, nil
}

func ctxErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
