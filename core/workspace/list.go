package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Entry 是一个可以列在侧边栏里的工作区。
// List 不打开数据库，只确认目录和三个库文件都在。
type Entry struct {
	ID string
}

// List 列出 root/workspaces 下能够打开的工作区，按标识排序。
// workspaces 目录不存在时返回空列表，不创建它。
// workspaces 本身是符号链接或不是目录时返回 [ErrPathEscape]，不跟着链接走。
// 符号链接、非目录、标识不合法，或缺少 kb、analytics、runs 的条目会被跳过。
func List(root string) ([]Entry, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("workspace: %w", ErrInvalidID)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("workspace: root: %w", err)
	}
	workspaces := filepath.Join(filepath.Clean(absRoot), dirWorkspaces)
	parent, err := os.Lstat(workspaces)
	if err != nil {
		if os.IsNotExist(err) {
			return []Entry{}, nil
		}
		return nil, fmt.Errorf("workspace: list: %w", err)
	}
	if parent.Mode()&os.ModeSymlink != 0 || !parent.IsDir() {
		return nil, fmt.Errorf("workspace: workspaces 不能作为列表根: %w", ErrPathEscape)
	}
	dirEntries, err := os.ReadDir(workspaces)
	if err != nil {
		return nil, fmt.Errorf("workspace: list: %w", err)
	}
	out := make([]Entry, 0, len(dirEntries))
	for _, ent := range dirEntries {
		id := ent.Name()
		if err := validateID(id); err != nil {
			continue
		}
		dir := filepath.Join(workspaces, id)
		info, err := os.Lstat(dir)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			continue
		}
		if err := ensureInside(workspaces, dir); err != nil {
			continue
		}
		if !hasStores(dir) {
			continue
		}
		out = append(out, Entry{ID: id})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func hasStores(dir string) bool {
	for _, name := range []string{fileKB, fileAnalytics, fileRuns} {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil || info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
	}
	return true
}
