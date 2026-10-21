package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"os"
	"strings"
	"testing"
)

func TestForceHelpMentionsNonUnix(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = old })

	err = run([]string{"-h"})
	if closeErr := w.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	var buf bytes.Buffer
	if _, copyErr := io.Copy(&buf, r); copyErr != nil {
		t.Fatal(copyErr)
	}
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("err=%v", err)
	}
	help := buf.String()
	if !strings.Contains(help, "-force") || !strings.Contains(help, "非 Unix 系统不支持") {
		t.Fatalf("帮助文字没有写明非 Unix 不支持:\n%s", help)
	}
}
