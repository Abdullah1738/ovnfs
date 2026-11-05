//go:build unix

package store

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// openRegular opens path only if it is a regular file: O_NOFOLLOW refuses a
// symlink swapped in at the leaf (whose target may be a file that must never
// be snapshotted) and O_NONBLOCK keeps a FIFO from blocking the open. It
// returns the file's size and device as fstat reported them.
func openRegular(path string) (*os.File, int64, uint64, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ELOOP) {
		return nil, 0, 0, errors.New("is a symlink, not a regular file")
	}
	if err != nil {
		return nil, 0, 0, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return nil, 0, 0, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		unix.Close(fd)
		return nil, 0, 0, errors.New("not a regular file")
	}
	if err := unix.SetNonblock(fd, false); err != nil {
		unix.Close(fd)
		return nil, 0, 0, err
	}
	return os.NewFile(uintptr(fd), path), st.Size, uint64(st.Dev), nil
}

// syncDir makes a rename in dir durable; failures leave only a crash window.
func syncDir(dir string) {
	if f, err := os.Open(dir); err == nil {
		f.Sync()
		f.Close()
	}
}
