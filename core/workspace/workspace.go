package workspace

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unicode"
)

const (
	dirWorkspaces = "workspaces"
	dirExports    = "exports"
	dirTmp        = "tmp"

	fileKB        = "kb.db"
	fileAnalytics = "analytics.db"
	fileSensitive = "sensitive.db"
	fileRuns      = "runs.db"
)

// 工作区标识和路径错误。调用方用 errors.Is 判断。
var (
	ErrInvalidID         = errors.New("workspace: invalid id")
	ErrPathEscape        = errors.New("workspace: path escapes workspace root")
	ErrExists            = errors.New("workspace: already exists")
	ErrNotFound          = errors.New("workspace: not found")
	ErrInvalidApproval   = errors.New("workspace: invalid approval id")
	ErrExportUnconfirmed = errors.New("workspace: export not confirmed")
)

// Handle 是一个已经打开的工作区。
// 启动时只打开 kb、analytics、runs。sensitive.db 不在这里，见 [Handle.OpenSensitive]。
type Handle struct {
	ID   string // 工作区标识
	Root string // 应用数据目录的绝对路径
	Dir  string // <Root>/workspaces/<ID> 的绝对路径

	KB        *sql.DB
	Analytics *sql.DB
	Runs      *sql.DB

	mu        sync.Mutex
	closed    bool
	nextGrant uint64
	grants    map[uint64]*exportGrant
	sensitive []*sql.DB
}

// DefaultRoot 返回本机应用数据目录。
// macOS 是 ~/Library/Application Support/Lantern，Linux 是 $XDG_DATA_HOME/lantern
// （未设置时是 ~/.local/share/lantern），Windows 是 %AppData%\Lantern。
func DefaultRoot() (string, error) {
	home, homeErr := os.UserHomeDir()
	dir, err := dataRoot(runtime.GOOS, home, os.Getenv("XDG_DATA_HOME"), os.Getenv("AppData"))
	if err != nil {
		if homeErr != nil {
			return "", fmt.Errorf("workspace: home directory: %w", homeErr)
		}
		return "", err
	}
	return dir, nil
}

func dataRoot(goos, home, xdg, appData string) (string, error) {
	switch goos {
	case "darwin":
		if home == "" {
			return "", errors.New("workspace: home directory is unknown")
		}
		return filepath.Join(home, "Library", "Application Support", "Lantern"), nil
	case "windows":
		if appData == "" {
			if home == "" {
				return "", errors.New("workspace: home directory is unknown")
			}
			appData = filepath.Join(home, "AppData", "Roaming")
		}
		return filepath.Join(appData, "Lantern"), nil
	default:
		if xdg != "" {
			return filepath.Join(xdg, "lantern"), nil
		}
		if home == "" {
			return "", errors.New("workspace: home directory is unknown")
		}
		return filepath.Join(home, ".local", "share", "lantern"), nil
	}
}

// Create 创建工作区目录、exports、tmp，并建好 kb、analytics、runs，再跑完这三个库的迁移。
// 不创建也不打开 sensitive.db。
// id 里出现 `../` 或其他会逃出 workspaces 的路径时返回 [ErrPathEscape]，不创建任何目录。
func Create(ctx context.Context, root, id string) (*Handle, error) {
	absRoot, dir, err := resolve(root, id)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, fmt.Errorf("workspace: create parent: %w", err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("workspace: id %q: %w", id, ErrExists)
		}
		return nil, fmt.Errorf("workspace: create %q: %w", id, err)
	}
	created := true
	defer func() {
		if created {
			_ = os.RemoveAll(dir)
		}
	}()
	if err := ensureInside(filepath.Dir(dir), dir); err != nil {
		return nil, fmt.Errorf("workspace: id %q: %w", id, err)
	}
	for _, sub := range []string{dirExports, dirTmp} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, fmt.Errorf("workspace: create %s: %w", sub, err)
		}
	}
	h, err := openAll(ctx, id, absRoot, dir)
	if err != nil {
		return nil, err
	}
	created = false
	return h, nil
}

// Open 打开已有工作区并补跑 kb、analytics、runs 上尚未应用的迁移。
// 目录或这三个库的文件不存在时返回 [ErrNotFound]。sensitive.db 缺失不算缺失，也不会被打开。
// 工作区目录若是指向 workspaces 之外的符号链接，返回 [ErrPathEscape]。
func Open(ctx context.Context, root, id string) (*Handle, error) {
	absRoot, dir, err := resolve(root, id)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("workspace: id %q: %w", id, ErrNotFound)
		}
		return nil, fmt.Errorf("workspace: open %q: %w", id, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace: id %q: %w", id, ErrNotFound)
	}
	if err := ensureInside(filepath.Dir(dir), dir); err != nil {
		return nil, fmt.Errorf("workspace: id %q: %w", id, err)
	}
	for _, name := range []string{fileKB, fileAnalytics, fileRuns} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("workspace: id %q: missing %s: %w", id, name, ErrNotFound)
			}
			return nil, fmt.Errorf("workspace: open %q: %w", id, err)
		}
	}
	for _, sub := range []string{dirExports, dirTmp} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, fmt.Errorf("workspace: open %s: %w", sub, err)
		}
	}
	return openAll(ctx, id, absRoot, dir)
}

// Close 关闭启动时打开的三个库，以及还没关掉的 sensitive 连接，并作废尚未使用的导出凭证。
func (h *Handle) Close() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	h.closed = true
	sensitive := h.sensitive
	h.sensitive = nil
	h.grants = nil
	h.mu.Unlock()

	var errs []error
	for _, db := range []*sql.DB{h.KB, h.Analytics, h.Runs} {
		if db == nil {
			continue
		}
		if err := db.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	for _, db := range sensitive {
		if db == nil {
			continue
		}
		if err := db.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func resolve(root, id string) (absRoot, dir string, err error) {
	if err := validateID(id); err != nil {
		return "", "", fmt.Errorf("workspace: id %q: %w", id, err)
	}
	if strings.TrimSpace(root) == "" {
		return "", "", fmt.Errorf("workspace: %w", ErrInvalidID)
	}
	absRoot, err = filepath.Abs(root)
	if err != nil {
		return "", "", fmt.Errorf("workspace: root: %w", err)
	}
	absRoot = filepath.Clean(absRoot)
	workspaces := filepath.Join(absRoot, dirWorkspaces)
	dir = filepath.Join(workspaces, id)
	rel, err := filepath.Rel(workspaces, dir)
	if err != nil {
		return "", "", fmt.Errorf("workspace: id %q: %w", id, err)
	}
	if rel != id || escapes(rel) {
		return "", "", fmt.Errorf("workspace: id %q: %w", id, ErrPathEscape)
	}
	return absRoot, dir, nil
}

// validateID 拒绝空标识、控制字符，以及任何含 `../` 或路径分隔符的标识。
// `../` 按路径穿越处理，返回 [ErrPathEscape]，调用方在碰文件系统之前就会失败。
func validateID(id string) error {
	if id == "" || len(id) > 128 {
		return ErrInvalidID
	}
	if id == "." || id == ".." {
		return ErrPathEscape
	}
	for _, r := range id {
		if r == 0 || unicode.IsControl(r) || unicode.IsSpace(r) {
			return ErrInvalidID
		}
		// 分隔符能拼出 `../` 或其他相对路径，一律按穿越拒绝。
		if r == '/' || r == '\\' {
			return ErrPathEscape
		}
	}
	if filepath.Clean(id) != id || filepath.Base(id) != id {
		return ErrPathEscape
	}
	return nil
}

func escapes(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ensureInside 在目录已经存在时再确认一次：解析符号链接后仍必须是 workspaces 下的这一层。
func ensureInside(workspacesDir, dir string) error {
	parent, err := filepath.EvalSymlinks(workspacesDir)
	if err != nil {
		return fmt.Errorf("workspace: resolve workspaces: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("workspace: resolve workspace: %w", err)
	}
	rel, err := filepath.Rel(parent, resolved)
	if err != nil {
		return err
	}
	if escapes(rel) || rel == "." || strings.ContainsRune(rel, filepath.Separator) {
		return ErrPathEscape
	}
	if filepath.Base(resolved) != filepath.Base(dir) {
		return ErrPathEscape
	}
	return nil
}
