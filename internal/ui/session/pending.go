package session

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/loveRyujin/lantern/core/approval"
	"github.com/loveRyujin/lantern/core/run"
	_ "modernc.org/sqlite"
)

// ReadPending 只读打开该工作区的 runs.db，返回仍有效的待确认单。
// 不调用 workspace.Open，因此不会跑 Recover，也不会追加 approval_restored。
func ReadPending(ctx context.Context, root, workspaceID string, now time.Time) ([]approval.Ticket, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := runsDBPath(root, workspaceID)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []approval.Ticket{}, nil
		}
		return nil, err
	}
	db, err := sql.Open("sqlite", readOnlyDSN(path))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return run.ListPending(ctx, db, workspaceID, now)
}

func runsDBPath(root, id string) (string, error) {
	if strings.TrimSpace(root) == "" || !validWorkspaceID(id) {
		return "", errors.New("session: 工作区路径不合法")
	}
	base := filepath.Join(root, "workspaces")
	dir := filepath.Join(base, id)
	rel, err := filepath.Rel(base, dir)
	if err != nil || rel != id || strings.HasPrefix(rel, "..") {
		return "", errors.New("session: 工作区路径不合法")
	}
	return filepath.Join(dir, "runs.db"), nil
}

func validWorkspaceID(id string) bool {
	if id == "" || len(id) > 128 || id == "." || id == ".." {
		return false
	}
	if strings.Contains(id, "/") || strings.Contains(id, `\`) || strings.Contains(id, "..") {
		return false
	}
	return true
}

func readOnlyDSN(path string) string {
	u := url.URL{
		Scheme: "file",
		Path:   filepath.ToSlash(path),
		RawQuery: url.Values{
			"mode":          {"ro"},
			"_busy_timeout": {"5000"},
		}.Encode(),
	}
	return u.String()
}
