package pyrt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnsureRebuildsWhenLockHashChanges 与 pyrt_test.TestEnsureRebuildsWhenBaseChanges 对称：
// 嵌入的锁文件字节变了，标记里的哈希对不上，就要重建 venv，并且不带 --clear。
func TestEnsureRebuildsWhenLockHashChanges(t *testing.T) {
	installFakePy(t, "3.12.1")
	logPath := filepath.Join(t.TempDir(), "log")
	t.Setenv("PYRT_FAKE_LOG", logPath)
	root := t.TempDir()
	cfg := Config{Root: root, SearchPaths: []string{}}
	if _, err := cfg.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}

	orig := append([]byte(nil), requirementsLock...)
	t.Cleanup(func() { requirementsLock = orig })
	requirementsLock = append(append([]byte(nil), orig...), []byte("\n# lock-hash-bump\n")...)
	oldSum := sha256.Sum256(orig)
	newSum := sha256.Sum256(requirementsLock)
	oldHash := hex.EncodeToString(oldSum[:])
	newHash := hex.EncodeToString(newSum[:])
	if oldHash == newHash {
		t.Fatal("锁文件内容没变，测不到重建")
	}

	rt, err := cfg.Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rt.Venv != filepath.Join(root, "pyenv", "venv") {
		t.Fatalf("Venv=%s", rt.Venv)
	}
	marker, err := os.ReadFile(filepath.Join(rt.Venv, markerName))
	if err != nil {
		t.Fatal(err)
	}
	text := string(marker)
	if !strings.Contains(text, "lock "+newHash) || strings.Contains(text, "lock "+oldHash) {
		t.Fatalf("标记没有换成新的锁哈希:\n%s", text)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(log), " -m venv ") != 2 || strings.Contains(string(log), "--clear") || strings.Count(string(log), " -m pip ") != 2 {
		t.Fatalf("锁哈希变化后应该重建，且不带 --clear:\n%s", log)
	}
}
