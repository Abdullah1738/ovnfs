//go:build darwin || linux

package source

import "os"

// readDir lists host directory dir sorted by name; entry keys come from their own stat data.
func readDir(dir string) ([]os.DirEntry, func(os.FileInfo) fileKey, error) {
	entries, err := os.ReadDir(dir)
	return entries, statKey, err
}
