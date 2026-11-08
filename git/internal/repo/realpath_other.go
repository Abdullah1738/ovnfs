//go:build !darwin

package repo

import "path/filepath"

// realPath returns the absolute physical path of an existing file, with
// every symlink resolved.
func realPath(p string) (string, error) {
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	return filepath.Abs(resolved)
}
