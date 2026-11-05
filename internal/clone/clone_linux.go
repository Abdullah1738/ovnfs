package clone

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// Reflink clones the open regular file in to the new path dst with mode, sharing its blocks.
// FICLONE shares the open inode's extents, so a path swapped after open cannot redirect the clone.
func Reflink(in *os.File, dst string, mode os.FileMode) error {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("clone to %s: %w", dst, err)
	}
	err = unix.IoctlFileClone(int(out.Fd()), int(in.Fd()))
	if err == nil {
		err = out.Chmod(mode)
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		return nil
	}
	if removeErr := os.Remove(dst); removeErr != nil {
		return fmt.Errorf("clone to %s: %w", dst, removeErr)
	}
	if unsupported(err) {
		return fmt.Errorf("clone to %s: %w: %w", dst, errors.ErrUnsupported, err)
	}
	return fmt.Errorf("clone to %s: %w", dst, err)
}
