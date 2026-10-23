//go:build linux && amd64

package sandbox

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func collectArtifacts(dir string, limit int64) ([]Artifact, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	if len(entries) > maxArtifacts {
		return nil, fmt.Errorf("too many artifacts (max 16)")
	}
	var artifacts []Artifact
	var total int64
	for _, entry := range entries {
		name := entry.Name()
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return nil, fmt.Errorf("artifact is not a regular file: %s", name)
		}
		fd, e := unix.Open(filepath.Join(dir, name), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
		if e != nil {
			return nil, e
		}
		file := os.NewFile(uintptr(fd), name)
		var stat unix.Stat_t
		e = unix.Fstat(fd, &stat)
		if e != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 {
			file.Close()
			return nil, fmt.Errorf("artifact must be regular with one link: %s", name)
		}
		data, e := io.ReadAll(io.LimitReader(file, artifactLimit(limit)+1))
		file.Close()
		if e != nil {
			return nil, e
		}
		total += int64(len(data))
		if total > maxArtifactsBytes {
			return nil, fmt.Errorf("artifacts exceed total 20 MiB")
		}
		art := Artifact{Name: name, Kind: ArtifactKind(strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")), Data: data}
		if e = ValidateArtifact(art, limit); e != nil {
			return nil, e
		}
		artifacts = append(artifacts, art)
	}
	return artifacts, nil
}
