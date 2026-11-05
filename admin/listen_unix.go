//go:build !windows

package admin

import (
	"errors"
	"os"
	"syscall"
)

// refused reports a dial error meaning no server is behind the socket file.
func refused(err error) bool { return errors.Is(err, syscall.ECONNREFUSED) }

// restrict makes the socket connectable only by its owner.
func restrict(path string) error { return os.Chmod(path, 0600) }
