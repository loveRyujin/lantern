// Package sandbox defines execution specs, artifact validation and stable errors.
//
// New provides a real Linux amd64 Landlock/seccomp runner for non-root callers
// without permitted/effective capabilities. Its current strict single-task
// profile supports one Python process and one thread (NProc 0/1). Fork, clone,
// process-group escape, networking and cross-process control are denied in the
// kernel; this is not a general multi-process Python environment.
//
// A trusted guardian handles parent liveness, sampled RSS/CPU/workspace budgets,
// process reaping, bounded artifact copying and directory cleanup. RSS sampling
// is not an instantaneous hard memory quota; Usage reports observed values and
// sampling gaps. Runtime files and approved extra directories are read-only.
//
// Missing runtime or isolation capabilities fail closed. Other platforms have
// no runner yet; Windows remains unsupported. Fake preserves the platform-neutral
// contract for callers whose platform integration is not available.
package sandbox
