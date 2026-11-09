//go:build !windows

package admin

import (
	"errors"
	"os"
	"syscall"
)

// Lock waits for an exclusive lock on the file at path, creating it; closing
// the returned file releases it. Listen and the CLI's daemon start serialize on it.
func Lock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// refused reports a dial error meaning no server is behind the socket file.
func refused(err error) bool { return errors.Is(err, syscall.ECONNREFUSED) }

// restrict makes the socket connectable only by its owner.
func restrict(path string) error { return os.Chmod(path, 0600) }
