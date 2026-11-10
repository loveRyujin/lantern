package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackageDoesNotImportMygo(t *testing.T) {
	assertNoMygo(t, ".")
	assertNoMygo(t, filepath.Join("..", "..", "..", "core"))
}

func assertNoMygo(t *testing.T, root string) {
	t.Helper()
	needle := "github.com/egoist/" + "mygo"
	err := filepath.WalkDir(root, func(path string, ent os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ent.IsDir() {
			if ent.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(ent.Name(), ".go") || strings.HasSuffix(ent.Name(), "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), needle) {
			t.Fatalf("%s 引用了 mygo", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
