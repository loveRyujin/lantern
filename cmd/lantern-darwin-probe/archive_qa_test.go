package main

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func qaExtractNativeArchive(data []byte, destination string) error {
	r := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := r.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		// git archive emits a global PAX record containing the source commit.
		// It is metadata, never a destination path or a materialized file.
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		if !filepath.IsLocal(h.Name) {
			return fmt.Errorf("archive path escape: %q", h.Name)
		}
		p := filepath.Join(destination, h.Name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
				return err
			}
			b, err := io.ReadAll(r)
			if err != nil {
				return err
			}
			if err := os.WriteFile(p, b, os.FileMode(h.Mode)&0777); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported archive entry type %d: %s", h.Typeflag, h.Name)
		}
	}
}

func TestDarwinProbeQAArchiveSkipsGitGlobalMetadata(t *testing.T) {
	var data bytes.Buffer
	w := tar.NewWriter(&data)
	if err := w.WriteHeader(&tar.Header{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader, Format: tar.FormatPAX, PAXRecords: map[string]string{"comment": "source-commit-metadata"}}); err != nil {
		t.Fatal(err)
	}
	content := []byte("package main\n")
	if err := w.WriteHeader(&tar.Header{Name: "cmd/probe.go", Typeflag: tar.TypeReg, Mode: 0600, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := qaExtractNativeArchive(data.Bytes(), root); err != nil {
		t.Fatalf("standard Git metadata prevented source extraction: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "cmd", "probe.go"))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("actual source was not extracted: %q %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "pax_global_header")); !os.IsNotExist(err) {
		t.Fatalf("metadata was materialized: %v", err)
	}
}

func TestDarwinProbeQAArchiveRejectsPathsAndLinks(t *testing.T) {
	for name, header := range map[string]tar.Header{
		"parent-path":   {Name: "../escape", Typeflag: tar.TypeReg, Mode: 0600},
		"absolute-path": {Name: "/escape", Typeflag: tar.TypeReg, Mode: 0600},
		"symlink":       {Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../escape"},
		"hardlink":      {Name: "link", Typeflag: tar.TypeLink, Linkname: "../escape"},
	} {
		t.Run(name, func(t *testing.T) {
			var data bytes.Buffer
			w := tar.NewWriter(&data)
			if err := w.WriteHeader(&header); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			parent := t.TempDir()
			root := filepath.Join(parent, "snapshot")
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			if err := qaExtractNativeArchive(data.Bytes(), root); err == nil {
				t.Fatal("unsafe materialized entry accepted")
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatalf("rejected entry wrote inside destination: %v %v", entries, err)
			}
			if _, err := os.Lstat(filepath.Join(parent, "escape")); !os.IsNotExist(err) {
				t.Fatalf("rejected entry escaped destination: %v", err)
			}
		})
	}
}
