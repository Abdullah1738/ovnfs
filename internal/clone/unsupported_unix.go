//go:build darwin || linux

package clone

import (
	"errors"

	"golang.org/x/sys/unix"
)

// unsupported reports clone failures that mean "this filesystem or pair of
// volumes cannot share blocks", the only cases that fall back to copying.
func unsupported(err error) bool {
	return errors.Is(err, unix.EXDEV) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) ||
		errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.ENOSYS)
}
