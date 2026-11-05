//go:build !darwin && !linux

package overlay

// cloneTree copies src to a new host path on platforms without a clone syscall.
func cloneTree(src, dst string) error { return copyTree(src, dst) }
