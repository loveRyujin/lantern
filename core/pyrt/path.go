package pyrt

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// errMissing 表示逐级 Lstat 时某一层还不存在。符号链接不是这种缺失。
var errMissing = errors.New("pyrt: 路径不存在")

// errSymlink 表示 O_NOFOLLOW 打开时最后一层是符号链接。
var errSymlink = errors.New("pyrt: 符号链接")

// testAfterPathCheck 在第一次路径检查通过之后、创建或删除任何目录之前运行。
// 测试用它把已经检查过的 pyenv 换成指向 Root 外的符号链接。生产代码保持 nil。
var testAfterPathCheck func()

// trustedRoot 把调用方给出的 Root 解析成真实目录，作为后面的信任起点。
// 祖先上的符号链接（macOS 的 /var、/tmp）在这里被展开，不再当成 path_escape。
func trustedRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("pyrt: Root: %w", err)
	}
	real, err := resolveExisting(filepath.Clean(abs))
	if err != nil {
		return "", fmt.Errorf("pyrt: Root: %w", err)
	}
	return real, nil
}

// guardWorkspace 先解析 Root，再只检查这个真实目录以下的 pyenv 和 venv。
func guardWorkspace(root string) error {
	real, err := trustedRoot(root)
	if err != nil {
		return err
	}
	return guardReal(real)
}

// guardReal 对真实 Root 以下逐级 Lstat。pyenv、venv 或其中间层是符号链接就拒绝。
// 解析后的位置也必须仍在这个真实 Root 内。本函数不创建、不删除。
func guardReal(real string) error {
	pyenv := filepath.Join(real, "pyenv")
	if err := guardOne(real, pyenv, true); err != nil {
		return err
	}
	venv := filepath.Join(pyenv, "venv")
	if _, err := os.Lstat(venv); err != nil {
		if os.IsNotExist(err) {
			return resolvedInside(real, venv)
		}
		return err
	}
	return guardOne(real, venv, false)
}

func guardOne(realRoot, path string, allowMissing bool) error {
	err := walkBelow(realRoot, path)
	if errors.Is(err, errMissing) {
		if !allowMissing {
			return err
		}
		return resolvedInside(realRoot, path)
	}
	if err != nil {
		return err
	}
	return resolvedInside(realRoot, path)
}

// walkBelow 只检查 realRoot 以下的每一层。realRoot 本身和它的祖先不在检查范围内。
func walkBelow(realRoot, path string) error {
	rel, err := filepath.Rel(filepath.Clean(realRoot), filepath.Clean(path))
	if err != nil || escapes(rel) {
		return pathEscape(path, "解析后位于 Root 之外")
	}
	if rel == "." {
		return nil
	}
	cur := filepath.Clean(realRoot)
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." || part == ".." {
			return pathEscape(path, "路径包含非法分量")
		}
		cur = filepath.Join(cur, part)
		info, lerr := os.Lstat(cur)
		if lerr != nil {
			if os.IsNotExist(lerr) {
				return errMissing
			}
			return lerr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return pathEscape(cur, "路径上有符号链接")
		}
		if !info.IsDir() {
			return fmt.Errorf("pyrt: %s 不是目录", cur)
		}
	}
	return nil
}

// ensureBelow 在真实 Root 下补齐 rel 的每一层。缺的目录用 Mkdir，不用 MkdirAll。
// 已有的符号链接直接拒绝，避免跟着链接把目录建到 Root 外面。
func ensureBelow(realRoot string, rel ...string) (string, error) {
	if err := mkdirMissingTail(realRoot); err != nil {
		return "", err
	}
	cur := filepath.Clean(realRoot)
	for _, part := range rel {
		if part == "" || part == "." || part == ".." {
			return "", pathEscape(cur, "路径包含非法分量")
		}
		parent := cur
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if os.IsNotExist(err) {
			pinfo, perr := os.Lstat(parent)
			if perr != nil {
				return "", perr
			}
			if pinfo.Mode()&os.ModeSymlink != 0 {
				return "", pathEscape(parent, "路径上有符号链接")
			}
			if !pinfo.IsDir() {
				return "", fmt.Errorf("pyrt: %s 不是目录", parent)
			}
			if mkErr := os.Mkdir(cur, 0o755); mkErr != nil && !os.IsExist(mkErr) {
				return "", mkErr
			}
			info, err = os.Lstat(cur)
			if err != nil {
				return "", err
			}
		} else if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", pathEscape(cur, "路径上有符号链接")
		}
		if !info.IsDir() {
			return "", fmt.Errorf("pyrt: %s 不是目录", cur)
		}
	}
	return cur, nil
}

// mkdirMissingTail 只创建 path 上还不存在的后缀。已存在的那一层如果自己是符号链接就拒绝；
// 更上面的祖先目录允许是符号链接，调用前应先把路径交给 trustedRoot。
func mkdirMissingTail(path string) error {
	path = filepath.Clean(path)
	var missing []string
	cur := path
	for {
		info, err := os.Lstat(cur)
		if err == nil {
			if len(missing) == 0 {
				if info.Mode()&os.ModeSymlink != 0 {
					return pathEscape(cur, "路径上有符号链接")
				}
				if !info.IsDir() {
					return fmt.Errorf("pyrt: %s 不是目录", cur)
				}
				return nil
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return pathEscape(cur, "路径上有符号链接")
			}
			if !info.IsDir() {
				return fmt.Errorf("pyrt: %s 不是目录", cur)
			}
			break
		}
		if !os.IsNotExist(err) {
			return err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return err
		}
		missing = append(missing, filepath.Base(cur))
		cur = parent
	}
	for i := len(missing) - 1; i >= 0; i-- {
		cur = filepath.Join(cur, missing[i])
		if err := os.Mkdir(cur, 0o755); err != nil && !os.IsExist(err) {
			return err
		}
		info, err := os.Lstat(cur)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return pathEscape(cur, "路径上有符号链接")
		}
		if !info.IsDir() {
			return fmt.Errorf("pyrt: %s 不是目录", cur)
		}
	}
	return nil
}

// resolvedInside 把已经存在的最长前缀做 EvalSymlinks，再拼上还不存在的后缀。
// 结果必须落在解析后的 Root 内。
func resolvedInside(realRoot, path string) error {
	resolved, err := resolveExisting(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(filepath.Clean(realRoot), resolved)
	if err != nil || escapes(rel) {
		return pathEscape(path, "解析后位于 Root 之外")
	}
	return nil
}

func resolveExisting(path string) (string, error) {
	path = filepath.Clean(path)
	var missing []string
	cur := path
	for {
		_, err := os.Lstat(cur)
		if err == nil {
			resolved, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return "", err
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}
		missing = append(missing, filepath.Base(cur))
		cur = parent
	}
}

func escapes(rel string) bool {
	if rel == "." {
		return false
	}
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel)
}

// removeRealDir 删除真实 Root 内的一个普通目录。
// 删除前再从 Root 往下 Lstat：任一层是符号链接，或解析后不在 Root 内，就返回 path_escape，不调用 RemoveAll。
func removeRealDir(realRoot, path string) error {
	if err := walkBelow(realRoot, path); err != nil {
		if errors.Is(err, errMissing) {
			return nil
		}
		return err
	}
	if err := resolvedInside(realRoot, path); err != nil {
		return err
	}
	if err := openDirNoFollow(path); err != nil {
		if errors.Is(err, errSymlink) {
			return pathEscape(path, "删除前发现符号链接")
		}
		return err
	}
	if err := walkBelow(realRoot, path); err != nil {
		if errors.Is(err, errMissing) {
			return nil
		}
		return err
	}
	if err := resolvedInside(realRoot, path); err != nil {
		return err
	}
	return os.RemoveAll(path)
}
