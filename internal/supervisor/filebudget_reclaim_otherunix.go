//go:build unix && !darwin

package supervisor

func reclaimSparseSpan(_ int) error { return nil }
