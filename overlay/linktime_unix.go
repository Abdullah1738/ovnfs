//go:build !windows

package overlay

import (
	"io/fs"
	"time"

	"golang.org/x/sys/unix"
)

// lchtimes sets p's own access and modification times, never following a
// final symlink; a zero time leaves that time unchanged, as for os.Chtimes.
func lchtimes(p string, atime, mtime time.Time) error {
	ts := []unix.Timespec{timespec(atime), timespec(mtime)}
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, p, ts, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return &fs.PathError{Op: "chtimes", Path: p, Err: err}
	}
	return nil
}

// timespec converts t for utimensat, omitting the zero time.
func timespec(t time.Time) unix.Timespec {
	if t.IsZero() {
		return unix.Timespec{Nsec: utimeOmit}
	}
	return unix.NsecToTimespec(t.UnixNano())
}
