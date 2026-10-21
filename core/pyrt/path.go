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

// guardWorkspace 拒绝 Root、Root/pyenv、已存在的 venv，以及这几条路径上任何一层符号链接。
// 解析现有前缀之后，路径必须仍在 lexical Root 里面。本函数不创建、不删除。
func guardWorkspace(root string) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("pyrt: Root: %w", err)
	}
	root = filepath.Clean(root)
	pyenv := filepath.Join(root, "pyenv")
	venv := filepath.Join(pyenv, "venv")
	for _, path := range []string{root, pyenv} {
		if err := guardOne(root, path, true); err != nil {
			return err
		}
	}
	if _, err := os.Lstat(venv); err != nil {
		if os.IsNotExist(err) {
			return resolvedInside(root, venv)
		}
		return err
	}
	return guardOne(root, venv, false)
}

func guardOne(root, path string, allowMissing bool) error {
	err := walkPath(path)
	if errors.Is(err, errMissing) {
		if !allowMissing {
			return err
		}
		return resolvedInside(root, path)
	}
	if err != nil {
		return err
	}
	return resolvedInside(root, path)
}

// walkPath 从卷根开始逐级 Lstat。任何一层是符号链接都返回 path_escape。
// 遇到还不存在的一层就停，不往下看，也不把它当成普通目录。
func walkPath(path string) error {
	vol, parts, err := splitAbs(path)
	if err != nil {
		return err
	}
	cur := volumePrefix(vol)
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return pathEscape(path, "路径包含非法分量")
		}
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err != nil {
			if os.IsNotExist(err) {
				return errMissing
			}
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

// ensureRealTree 补齐还不存在的目录。每一层都先 Lstat：已有的符号链接直接拒绝，不用 MkdirAll。
func ensureRealTree(path string) error {
	vol, parts, err := splitAbs(path)
	if err != nil {
		return err
	}
	cur := volumePrefix(vol)
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return pathEscape(path, "路径包含非法分量")
		}
		parent := cur
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if os.IsNotExist(err) {
			pinfo, perr := os.Lstat(parent)
			if perr != nil {
				return perr
			}
			if pinfo.Mode()&os.ModeSymlink != 0 {
				return pathEscape(parent, "路径上有符号链接")
			}
			if !pinfo.IsDir() {
				return fmt.Errorf("pyrt: %s 不是目录", parent)
			}
			if mkErr := os.Mkdir(cur, 0o755); mkErr != nil && !os.IsExist(mkErr) {
				return mkErr
			}
			info, err = os.Lstat(cur)
			if err != nil {
				return err
			}
		} else if err != nil {
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
// 结果必须落在 lexical Root 内。Root 外面的符号链接目标在这里被拒绝。
func resolvedInside(root, path string) error {
	root = filepath.Clean(root)
	resolved, err := resolveExisting(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, resolved)
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

func splitAbs(path string) (string, []string, error) {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return "", nil, fmt.Errorf("pyrt: %s 不是绝对路径", path)
	}
	vol := filepath.VolumeName(path)
	rest := strings.TrimPrefix(path[len(vol):], string(filepath.Separator))
	if rest == "" {
		return vol, nil, nil
	}
	return vol, strings.Split(rest, string(filepath.Separator)), nil
}

func volumePrefix(vol string) string {
	if vol == "" {
		return string(filepath.Separator)
	}
	if strings.HasSuffix(vol, string(filepath.Separator)) {
		return vol
	}
	return vol + string(filepath.Separator)
}

// removeRealDir 删除一个确认过的普通目录。
// 删除前再走一遍 Lstat：任一层是符号链接，或解析后不在 Root 内，就返回 path_escape，不调用 RemoveAll。
func removeRealDir(root, path string) error {
	if err := walkPath(path); err != nil {
		if errors.Is(err, errMissing) {
			return nil
		}
		return err
	}
	if err := resolvedInside(root, path); err != nil {
		return err
	}
	if err := openDirNoFollow(path); err != nil {
		if errors.Is(err, errSymlink) {
			return pathEscape(path, "删除前发现符号链接")
		}
		return err
	}
	if err := walkPath(path); err != nil {
		if errors.Is(err, errMissing) {
			return nil
		}
		return err
	}
	if err := resolvedInside(root, path); err != nil {
		return err
	}
	return os.RemoveAll(path)
}
