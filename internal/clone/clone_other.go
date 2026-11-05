//go:build !darwin && !linux

package clone

import (
	"errors"
	"fmt"
	"os"
)

// Reflink always reports errors.ErrUnsupported: these platforms have no supported clone syscall.
func Reflink(_ *os.File, dst string, _ os.FileMode) error {
	return fmt.Errorf("clone to %s: %w", dst, errors.ErrUnsupported)
}
