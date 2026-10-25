package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSupervisionQAControlDirectoryIsExclusive(t *testing.T) {
	parent := t.TempDir()
	control := filepath.Join(parent, "events")
	if err := createSupervisionControl(control); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(control, "old-pid.json")
	want := []byte(`{"pid":424242,"note":"prior user data"}`)
	if err := os.WriteFile(marker, want, 0600); err != nil {
		t.Fatal(err)
	}
	if err := createSupervisionControl(control); err == nil {
		t.Fatal("existing control directory was silently reused")
	}
	got, err := os.ReadFile(marker)
	if err != nil || string(got) != string(want) {
		t.Fatalf("rejected reuse changed prior data: %q %v", got, err)
	}
	entries, err := os.ReadDir(control)
	if err != nil || len(entries) != 1 || entries[0].Name() != "old-pid.json" {
		t.Fatalf("rejected reuse created actor data: %v %v", entries, err)
	}
}
