package clone

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// Reflink clones the open regular file in to the new path dst with mode, sharing its blocks.
// fclonefileat clones the already-open inode, so a path swapped after open cannot redirect the clone.
func Reflink(in *os.File, dst string, mode os.FileMode) error {
	err := unix.Fclonefileat(int(in.Fd()), unix.AT_FDCWD, dst, unix.CLONE_NOFOLLOW)
	if unsupported(err) {
		return fmt.Errorf("clone to %s: %w: %w", dst, errors.ErrUnsupported, err)
	}
	if err != nil {
		return fmt.Errorf("clone to %s: %w", dst, err)
	}
	if err := os.Chmod(dst, mode); err != nil {
		os.Remove(dst)
		return fmt.Errorf("clone to %s: %w", dst, err)
	}
	return nil
}
