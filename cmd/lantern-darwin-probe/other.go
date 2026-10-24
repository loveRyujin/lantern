//go:build !darwin

package main

import "context"

func internalMain([]string) (bool, int) { return false, 0 }
func runProbe(_ context.Context, _ options) Report {
	r := newReport()
	r.Error = "actual macOS is required; no host fallback or cross-build execution"
	return r
}
