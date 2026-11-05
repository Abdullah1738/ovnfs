//go:build !linux && !darwin && !windows

package store

// renameNoReplace renames old to new unless new exists, reporting whether it did.
func renameNoReplace(old, new string) (bool, error) { return renameChecked(old, new) }
