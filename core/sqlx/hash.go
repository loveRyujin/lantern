package sqlx

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// ErrFileChanged 表示只读查询前后，主库或 -wal 的内容变了，或者多出了不该有的 -wal。
// 错误文本不带路径。
var ErrFileChanged = errors.New("sqlx: 分析库文件在只读查询后发生了变化")

// Snap 是一次文件指纹。路径都是 EvalSymlinks 之后的结果，-wal 同时看解析后的文件名和调用方给出的路径。
// -shm 不在指纹里。它是 WAL 的共享内存索引，只读连接为了和写入者协调也会改它，里面没有业务行。
// 钉住主库字节、钉住执行前已有的 -wal、并且不允许新建 -wal，能读到的内容就不会变。
type Snap struct {
	Resolved string
	MainSum  [32]byte
	Sides    []Side
}

// Side 是主库旁边可能出现的 -wal。执行前不存在的，执行后也不能出现。
type Side struct {
	Path   string
	Exists bool
	Sum    [32]byte
}

// Resolve 返回 path 的绝对路径再做 EvalSymlinks。比较两条路径之前都要走这里。
func Resolve(path string) (string, error) {
	if path == "" {
		return "", errors.New("sqlx: 分析库路径为空")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("sqlx: 分析库路径: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("sqlx: 分析库路径: %w", err)
	}
	return resolved, nil
}

// Snapshot 读取主库和相关 -wal 的 SHA-256。文件要已经存在。
func Snapshot(path string) (Snap, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Snap{}, fmt.Errorf("sqlx: 分析库路径: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return Snap{}, fmt.Errorf("sqlx: 分析库路径: %w", err)
	}
	sum, err := hashFile(resolved)
	if err != nil {
		return Snap{}, err
	}
	names := []string{resolved + "-wal"}
	if abs != resolved {
		names = append(names, abs+"-wal")
	}
	slices.Sort(names)
	names = slices.Compact(names)
	sides := make([]Side, 0, len(names))
	for _, name := range names {
		side, err := hashSide(name)
		if err != nil {
			return Snap{}, err
		}
		sides = append(sides, side)
	}
	return Snap{Resolved: resolved, MainSum: sum, Sides: sides}, nil
}

// Unchanged 报告 after 是否仍是同一次只读查询之前的那个库。
// 主库路径按 EvalSymlinks 之后的结果比较。执行前已有的 -wal 哈希必须相同；
// 执行前没有的 -wal，执行后不能出现。
func (before Snap) Unchanged(after Snap) error {
	if before.Resolved == "" || before.Resolved != after.Resolved {
		return ErrFileChanged
	}
	if before.MainSum != after.MainSum {
		return ErrFileChanged
	}
	if len(before.Sides) != len(after.Sides) {
		return ErrFileChanged
	}
	for i := range before.Sides {
		b := before.Sides[i]
		a := after.Sides[i]
		if b.Path != a.Path || b.Exists != a.Exists {
			return ErrFileChanged
		}
		if b.Exists && b.Sum != a.Sum {
			return ErrFileChanged
		}
	}
	return nil
}

func hashFile(path string) ([32]byte, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return [32]byte{}, fmt.Errorf("sqlx: 读取分析库: %w", err)
	}
	return sha256.Sum256(body), nil
}

func hashSide(path string) (Side, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Side{Path: path}, nil
		}
		return Side{}, fmt.Errorf("sqlx: 读取分析库: %w", err)
	}
	return Side{Path: path, Exists: true, Sum: sha256.Sum256(body)}, nil
}
