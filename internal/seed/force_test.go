package seed

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loveRyujin/lantern/core/workspace"
)

func TestConsecutiveSeed(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	first, err := Run(ctx, Options{Root: root, Seed: DefaultSeed})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Run(ctx, Options{Root: root, Seed: DefaultSeed, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if first.Hash == "" || first.Hash != second.Hash {
		t.Fatalf("consecutive seed hash %s vs %s", first.Hash, second.Hash)
	}
	for i := range first.Workspaces {
		if first.Workspaces[i].Hash != second.Workspaces[i].Hash {
			t.Fatalf("%s hash changed", first.Workspaces[i].ID)
		}
	}

	h, err := workspace.Open(ctx, root, "星禾")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	if _, err := writeSeedApproval(ctx, h); err != nil {
		t.Fatal(err)
	}
	if _, err := writeSeedApproval(ctx, h); err != nil {
		t.Fatalf("second approval on the same workspace: %v", err)
	}
	var fixed, prefixed int
	if err := h.Runs.QueryRowContext(ctx, `SELECT COUNT(*) FROM approvals WHERE id = 'lantern-seed'`).Scan(&fixed); err != nil {
		t.Fatal(err)
	}
	if fixed != 0 {
		t.Fatalf("fixed approval id rows = %d", fixed)
	}
	if err := h.Runs.QueryRowContext(ctx, `SELECT COUNT(*) FROM approvals WHERE id LIKE 'lantern-seed-%'`).Scan(&prefixed); err != nil {
		t.Fatal(err)
	}
	if prefixed != 3 {
		t.Fatalf("seed approvals = %d, want 3", prefixed)
	}
}

func TestForceKeepsExistingWhenLaterWorkspaceFails(t *testing.T) {
	root := t.TempDir()
	writeSeedMarker(t, filepath.Join(root, "workspaces", "星禾", "marker.txt"), "old-xh")
	writeSeedMarker(t, filepath.Join(root, "workspaces", "云杉", "marker.txt"), "old-ys")

	orig := seedProfile
	seedProfile = func(ctx context.Context, root string, seedN uint64, profile profile) (Summary, error) {
		if profile.ID == "云杉" {
			return Summary{}, errors.New("云杉生成失败")
		}
		return Summary{ID: profile.ID, Hash: "staged"}, nil
	}
	t.Cleanup(func() { seedProfile = orig })

	_, err := Run(t.Context(), Options{Root: root, Seed: DefaultSeed, Force: true})
	if err == nil || !strings.Contains(err.Error(), "云杉生成失败") {
		t.Fatalf("Run: %v", err)
	}
	assertSeedMarker(t, filepath.Join(root, "workspaces", "星禾", "marker.txt"), "old-xh")
	assertSeedMarker(t, filepath.Join(root, "workspaces", "云杉", "marker.txt"), "old-ys")
	assertSeedScratchGone(t, root)
}

func TestForceDoesNotReplaceWhenStagingIsIncomplete(t *testing.T) {
	root := t.TempDir()
	writeSeedMarker(t, filepath.Join(root, "workspaces", "星禾", "marker.txt"), "old-xh")
	writeSeedMarker(t, filepath.Join(root, "workspaces", "云杉", "marker.txt"), "old-ys")

	orig := seedProfile
	seedProfile = func(ctx context.Context, root string, seedN uint64, profile profile) (Summary, error) {
		return Summary{ID: profile.ID, Hash: "staged"}, nil
	}
	t.Cleanup(func() { seedProfile = orig })

	_, err := Run(t.Context(), Options{Root: root, Seed: DefaultSeed, Force: true})
	if !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("Run: %v", err)
	}
	assertSeedMarker(t, filepath.Join(root, "workspaces", "星禾", "marker.txt"), "old-xh")
	assertSeedMarker(t, filepath.Join(root, "workspaces", "云杉", "marker.txt"), "old-ys")
	assertSeedScratchGone(t, root)
}

func TestForceSymlinkDoesNotGenerateOrFollow(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "keep.txt")
	writeSeedMarker(t, secret, "keep")
	if err := os.Symlink(outside, filepath.Join(root, "workspaces")); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(root, ".lantern-seed-stale", "phone.txt")
	writeSeedMarker(t, stale, "13800000000")
	if err := os.Symlink(outside, filepath.Join(root, ".lantern-seed-link")); err != nil {
		t.Fatal(err)
	}

	orig := seedProfile
	called := false
	seedProfile = func(ctx context.Context, root string, seedN uint64, profile profile) (Summary, error) {
		called = true
		return Summary{}, errors.New("不应生成")
	}
	t.Cleanup(func() { seedProfile = orig })

	_, err := Run(t.Context(), Options{Root: root, Seed: DefaultSeed, Force: true})
	if !errors.Is(err, workspace.ErrPathEscape) {
		t.Fatalf("Run: %v", err)
	}
	if called {
		t.Fatal("symlink rejection still generated a workspace")
	}
	assertSeedMarker(t, secret, "keep")
	if _, err := os.Stat(filepath.Dir(stale)); !os.IsNotExist(err) {
		t.Fatalf("stale scratch still present: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, ".lantern-seed-link")); !os.IsNotExist(err) {
		t.Fatalf("scratch symlink still present: %v", err)
	}
	info, err := os.Lstat(filepath.Join(root, "workspaces"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("workspaces symlink was replaced")
	}
}

func writeSeedMarker(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertSeedMarker(t *testing.T, path, want string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != want {
		t.Fatalf("%s = %q, want %q", path, body, want)
	}
}

func assertSeedScratchGone(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".lantern-seed-") {
			t.Fatalf("scratch left behind: %s", entry.Name())
		}
	}
}
