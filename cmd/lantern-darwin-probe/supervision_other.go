//go:build !darwin

package main

import "context"

func runSupervision(context.Context, options) SupervisionReport {
	r := newSupervisionReport()
	r.Error = "actual macOS required; no fallback"
	return r
}
