package session

import (
	"os"
	"strings"
	"testing"
)

func TestPackageDoesNotImportMygo(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	needle := "github.com/egoist/" + "mygo"
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".go") {
			continue
		}
		data, err := os.ReadFile(ent.Name())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), needle) {
			t.Fatalf("%s 引用了 mygo", ent.Name())
		}
	}
}
