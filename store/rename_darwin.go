package store

import (
	"errors"

	"golang.org/x/sys/unix"
)

// renameNoReplace renames old to new unless new exists, reporting whether it did.
func renameNoReplace(old, new string) (bool, error) {
	err := unix.RenamexNp(old, new, unix.RENAME_EXCL)
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, unix.EEXIST):
		return true, nil
	case errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EINVAL):
		// The filesystem lacks RENAME_EXCL.
		return renameChecked(old, new)
	}
	return false, err
}
