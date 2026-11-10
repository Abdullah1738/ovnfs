//go:build darwin

package checkout

import (
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
)

// flock interoperates with legacy daemons, but F_GETLK reports its owner as
// -1. A separate POSIX record lock supplies a kernel PID without inspecting
// arbitrary descriptors. No other descriptor of this file is opened in the
// holder process: closing one would release its POSIX record locks.
func lockProof(state string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(state, "state.owner.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	lock := unix.Flock_t{Type: unix.F_WRLCK, Whence: 0, Len: 1}
	if err := unix.FcntlFlock(f.Fd(), unix.F_SETLK, &lock); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func proofOwner(state string) (int, error) {
	f, err := os.OpenFile(filepath.Join(state, "state.owner.lock"), os.O_RDWR, 0)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	lock := unix.Flock_t{Type: unix.F_WRLCK, Whence: 0, Len: 1}
	if err := unix.FcntlFlock(f.Fd(), unix.F_GETLK, &lock); err != nil {
		return 0, err
	}
	if lock.Type == unix.F_UNLCK || lock.Pid <= 0 {
		return 0, nil
	}
	return int(lock.Pid), nil
}
