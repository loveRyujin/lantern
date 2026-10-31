//go:build darwin

package supervisor

func reclaimSparseSpan(fd int) error {
	return punchUnwrittenSpan(fd, 4096, (1<<20)-4096)
}
