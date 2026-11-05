//go:build !darwin

package git

// cloneIndexFile selects the portable streaming copy outside Darwin.
func cloneIndexFile(src, dst string) (bool, error) { return false, nil }
