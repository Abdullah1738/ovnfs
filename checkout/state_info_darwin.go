//go:build darwin

package checkout

import (
	"golang.org/x/sys/unix"
	"os"
)

func stateLockInfo(f *os.File) (int, bool, error) {
	lock := unix.Flock_t{Type: unix.F_WRLCK, Whence: 0}
	if err := unix.FcntlFlock(f.Fd(), unix.F_GETLK, &lock); err != nil {
		return 0, false, err
	}
	return int(lock.Pid), lock.Type != unix.F_UNLCK, nil
}
