package store

import (
	"errors"

	"golang.org/x/sys/unix"
)

// renameNoReplace renames old to new unless new exists, reporting whether it did.
func renameNoReplace(old, new string) (bool, error) {
	err := unix.Renameat2(unix.AT_FDCWD, old, unix.AT_FDCWD, new, unix.RENAME_NOREPLACE)
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, unix.EEXIST):
		return true, nil
	case errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOSYS), errors.Is(err, unix.ENOTSUP):
		// The filesystem or kernel lacks RENAME_NOREPLACE.
		return renameChecked(old, new)
	}
	return false, err
}
