package overlay

import (
	"errors"

	"golang.org/x/sys/unix"
)

// cloneTree copies src to the new path dst. On APFS, clonefile(2) shares the
// data blocks (a directory is cloned with its whole hierarchy), making
// copy-up O(1) in file size; across volumes it falls back to a byte copy.
func cloneTree(src, dst string) error {
	err := unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW)
	if errors.Is(err, unix.EXDEV) || errors.Is(err, unix.ENOTSUP) {
		return copyTree(src, dst)
	}
	return err
}
