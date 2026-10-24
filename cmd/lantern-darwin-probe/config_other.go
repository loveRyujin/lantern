//go:build !linux && !darwin

package main

import (
	"fmt"
	"os"
)

func openConfig(path string) (*os.File, error) {
	s, e := os.Lstat(path)
	if e != nil {
		return nil, e
	}
	if !s.Mode().IsRegular() {
		return nil, fmt.Errorf("runtime config must be regular")
	}
	return os.Open(path)
}
