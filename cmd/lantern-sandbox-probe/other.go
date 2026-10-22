//go:build !linux || !amd64

package main

func run(_ []string) report {
	return report{Backend: "landlock-seccomp", Status: "unsupported", Error: "fixed probe supports Linux amd64 only; no unsandboxed fallback"}
}
