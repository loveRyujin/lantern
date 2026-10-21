package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// scratchPrefix 是数据目录下临时目录的前缀。
// 生成和替换都用它。CleanScratch 会删掉这个前缀下上次中断留下的目录。
const scratchPrefix = ".lantern-seed-"

// renamePath 换目录。测试替换它，用来模拟替换进行到一半时失败。
var renamePath = os.Rename

// Remove 删除 <root>/workspaces/<id>。
// 目录不存在时返回 nil，也不创建 workspaces。
// workspaces 或工作区本身是符号链接、不是目录，或者解析符号链接后不再是
// workspaces 的直接子目录时，返回 [ErrPathEscape]，不删除任何文件。
func Remove(root, id string) error {
	dir, exists, err := inspect(root, id)
	if err != nil || !exists {
		return err
	}
	if err := ensureInside(filepath.Dir(dir), dir); err != nil {
		return fmt.Errorf("workspace: 删除 %q: %w", id, err)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("workspace: 删除 %q: %w", id, err)
	}
	return nil
}

// Replaceable 检查 id 能不能被安全删除或替换。
// 工作区目录不存在时返回 nil。这个函数不创建、也不删除文件。
func Replaceable(root, id string) error {
	_, _, err := inspect(root, id)
	return err
}

// CleanScratch 删除 root 下上次中断留下的临时目录。
// 只处理以 `.lantern-seed-` 开头的名字，不会动 workspaces。
// 名字本身是符号链接时只去掉链接，不跟着进目标目录。
// 不要在 [Install] 进行中调用，否则会把正在使用的临时目录一起删掉。
func CleanScratch(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("workspace: 清理临时目录: %w", err)
	}
	var errs []error
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, scratchPrefix) {
			continue
		}
		path := filepath.Join(root, name)
		info, statErr := os.Lstat(path)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				continue
			}
			errs = append(errs, statErr)
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
				errs = append(errs, rmErr)
			}
			continue
		}
		if rmErr := os.RemoveAll(path); rmErr != nil {
			errs = append(errs, rmErr)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("workspace: 清理临时目录: %w", errors.Join(errs...))
	}
	return nil
}

// ScratchDir 在 root 下新建一个临时目录。root 不存在时会创建。
func ScratchDir(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("workspace: 临时目录: %w", ErrInvalidID)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("workspace: 临时目录: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", fmt.Errorf("workspace: 临时目录: %w", err)
	}
	dir, err := os.MkdirTemp(abs, scratchPrefix)
	if err != nil {
		return "", fmt.Errorf("workspace: 临时目录: %w", err)
	}
	return dir, nil
}

// Install 把 srcRoot 里已经建好的工作区换到 dstRoot。
// 每个 id 都会先检查：源必须是真实目录；目标若是符号链接或会逃出 workspaces，
// 返回 [ErrPathEscape]。检查都通过之前不会改动目标。
// 替换时先把旧目录挪到临时目录，再把新目录换上。中途失败会把已经换上的目录移回去。
// 成功后这些工作区不再留在 srcRoot。返回装好之后的目录，顺序与 ids 相同。
//
// 检查通过之后、renamePath 之前有一段时间窗。这是本机单用户的开发工具，接受这个前提：
// 如果这中间有人把 workspaces 换成符号链接，rename 会跟着链接走，这里不再复查。
func Install(dstRoot, srcRoot string, ids []string) (dirs []string, err error) {
	plans, err := planInstall(dstRoot, srcRoot, ids)
	if err != nil {
		return nil, err
	}
	hold, err := ScratchDir(dstRoot)
	if err != nil {
		return nil, err
	}
	var done []relocation
	defer func() {
		if err != nil {
			if undoErr := undo(done); undoErr != nil {
				err = fmt.Errorf("%w（恢复旧目录失败: %v）", err, undoErr)
				return
			}
		}
		if rmErr := os.RemoveAll(hold); rmErr != nil && err == nil {
			err = fmt.Errorf("workspace: 清理替换前的工作区: %w", rmErr)
		}
	}()

	for _, plan := range plans {
		rel := relocation{id: plan.id, dest: plan.dest, src: plan.src}
		if plan.destExists {
			rel.backup = filepath.Join(hold, plan.id)
			if err = renamePath(plan.dest, rel.backup); err != nil {
				return nil, fmt.Errorf("workspace: 挪走 %q: %w", plan.id, err)
			}
		} else {
			// 先确认父目录不是符号链接，再创建。否则 MkdirAll 会跟着链接把目录建到别处。
			if _, _, inspectErr := inspect(dstRoot, plan.id); inspectErr != nil {
				return nil, inspectErr
			}
			if err = os.MkdirAll(filepath.Dir(plan.dest), 0o755); err != nil {
				return nil, fmt.Errorf("workspace: 创建工作区目录: %w", err)
			}
		}
		if _, exists, inspectErr := inspect(dstRoot, plan.id); inspectErr != nil {
			done = append(done, rel)
			return nil, inspectErr
		} else if exists {
			done = append(done, rel)
			return nil, fmt.Errorf("workspace: 装入 %q: 目标仍在", plan.id)
		}
		if err = renamePath(plan.src, plan.dest); err != nil {
			done = append(done, rel)
			return nil, fmt.Errorf("workspace: 装入 %q: %w", plan.id, err)
		}
		if err = ensureInside(filepath.Dir(plan.dest), plan.dest); err != nil {
			done = append(done, rel)
			return nil, fmt.Errorf("workspace: 装入 %q: %w", plan.id, err)
		}
		done = append(done, rel)
	}

	dirs = make([]string, len(plans))
	for i, plan := range plans {
		dirs[i] = plan.dest
	}
	return dirs, nil
}

type installPlan struct {
	id         string
	dest       string
	src        string
	destExists bool
}

type relocation struct {
	id     string
	dest   string
	src    string
	backup string
}

func planInstall(dstRoot, srcRoot string, ids []string) ([]installPlan, error) {
	if len(ids) == 0 {
		return nil, errors.New("workspace: 没有要安装的工作区")
	}
	seen := make(map[string]struct{}, len(ids))
	plans := make([]installPlan, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			return nil, fmt.Errorf("workspace: 重复的工作区 %q", id)
		}
		seen[id] = struct{}{}
		dest, destExists, err := inspect(dstRoot, id)
		if err != nil {
			return nil, err
		}
		src, srcExists, err := inspect(srcRoot, id)
		if err != nil {
			return nil, err
		}
		if !srcExists {
			return nil, fmt.Errorf("workspace: 安装 %q: %w", id, ErrNotFound)
		}
		overlap, err := overlaps(src, dest)
		if err != nil {
			return nil, err
		}
		if overlap {
			return nil, fmt.Errorf("workspace: 安装 %q: 源目录和目标目录重叠", id)
		}
		plans = append(plans, installPlan{
			id: id, dest: dest, src: src, destExists: destExists,
		})
	}
	return plans, nil
}

func undo(done []relocation) error {
	var errs []error
	for i := len(done) - 1; i >= 0; i-- {
		rel := done[i]
		_, err := os.Lstat(rel.dest)
		if err == nil {
			if mvErr := renamePath(rel.dest, rel.src); mvErr != nil {
				errs = append(errs, mvErr)
				continue
			}
		} else if !os.IsNotExist(err) {
			errs = append(errs, err)
			continue
		}
		if rel.backup == "" {
			continue
		}
		if _, err := os.Lstat(rel.backup); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			errs = append(errs, err)
			continue
		}
		if mvErr := renamePath(rel.backup, rel.dest); mvErr != nil {
			errs = append(errs, mvErr)
		}
	}
	return errors.Join(errs...)
}

// inspect 确认工作区要么不存在，要么是 workspaces 下的真实目录。
// Lstat 不跟随最后一级：workspaces 或工作区是符号链接时直接拒绝。
// 只靠 EvalSymlinks 不够，父目录本身是链接时，解析后的子目录看起来仍在父目录里面。
// 目录真实存在时再走 ensureInside，和 Create、Open 用同一条边界。
func inspect(root, id string) (dir string, exists bool, err error) {
	absRoot, dir, err := resolve(root, id)
	if err != nil {
		return "", false, err
	}
	workspaces := filepath.Join(absRoot, dirWorkspaces)
	parent, err := os.Lstat(workspaces)
	if err != nil {
		if os.IsNotExist(err) {
			return dir, false, nil
		}
		return "", false, fmt.Errorf("workspace: %q: %w", id, err)
	}
	if parent.Mode()&os.ModeSymlink != 0 {
		return "", false, fmt.Errorf("workspace: workspaces 是符号链接，拒绝删除: %w", ErrPathEscape)
	}
	if !parent.IsDir() {
		return "", false, fmt.Errorf("workspace: workspaces 不是目录，拒绝删除: %w", ErrPathEscape)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return dir, false, nil
		}
		return "", false, fmt.Errorf("workspace: %q: %w", id, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", false, fmt.Errorf("workspace: 工作区 %q 是符号链接，拒绝删除: %w", id, ErrPathEscape)
	}
	if !info.IsDir() {
		return "", false, fmt.Errorf("workspace: 工作区 %q 不是目录，拒绝删除: %w", id, ErrPathEscape)
	}
	if err := ensureInside(workspaces, dir); err != nil {
		return "", false, fmt.Errorf("workspace: %q: %w", id, err)
	}
	return dir, true, nil
}

func overlaps(a, b string) (bool, error) {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if a == b {
		return true, nil
	}
	for _, pair := range [][2]string{{a, b}, {b, a}} {
		rel, err := filepath.Rel(pair[0], pair[1])
		if err != nil {
			return false, err
		}
		if rel == "." || !escapes(rel) {
			return true, nil
		}
	}
	return false, nil
}
