//go:build linux || windows

package clone

import (
	"errors"
	"fmt"
	"os"
)

// Reflink clones the open regular file in to the new path dst with mode, sharing its blocks.
func Reflink(in *os.File, dst string, mode os.FileMode) error {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("clone to %s: %w", dst, err)
	}
	err = cloneInto(out, in)
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
