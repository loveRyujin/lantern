//go:build !darwin

package main

import (
	"encoding/json"
	"io"
	"os"
	"testing"
)

func TestFileAPIUnsupportedOffDarwin(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = write
	code := fileAPICLI(nil)
	write.Close()
	os.Stdout = old
	body, err := io.ReadAll(read)
	if err != nil {
		t.Fatal(err)
	}
	var report fileAPIReport
	if json.Unmarshal(body, &report) != nil || code != 1 || report.Schema != fileAPISchema || report.Status != "unsupported" || report.Error == "" {
		t.Fatalf("non-Darwin files-api did not fail closed: code=%d body=%s", code, body)
	}
}
