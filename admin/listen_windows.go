//go:build windows

package admin

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// Lock waits for an exclusive lock on the file at path, creating it; closing
// the returned file releases it. Listen and the CLI's daemon start serialize on it.
func Lock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	var overlapped windows.Overlapped
	// Blocking: no LOCKFILE_FAIL_IMMEDIATELY.
	if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &overlapped); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// refused reports a dial error meaning no server is behind the socket file.
func refused(err error) bool { return errors.Is(err, windows.WSAECONNREFUSED) }

// restrict leaves access to the socket file's ACL, inherited from its
// directory (mode bits do not apply on Windows); CheckPeer enforces the user.
func restrict(string) error { return nil }
