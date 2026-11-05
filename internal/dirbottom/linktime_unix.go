//go:build !windows

package dirbottom

import (
	"time"

	"golang.org/x/sys/unix"
)

// linkTime restores a symlink's own timestamps without following its target.
func linkTime(path string, mtime time.Time) error {
	t := unix.NsecToTimespec(mtime.UnixNano())
	return unix.UtimesNanoAt(unix.AT_FDCWD, path, []unix.Timespec{t, t}, unix.AT_SYMLINK_NOFOLLOW)
}
