//go:build !windows

package overlay

import (
	"time"

	"golang.org/x/sys/unix"
)

// linkTime restores a symlink's own timestamps, never following its target.
func linkTime(p string, mtime time.Time) error {
	t := unix.NsecToTimespec(mtime.UnixNano())
	return unix.UtimesNanoAt(unix.AT_FDCWD, p, []unix.Timespec{t, t}, unix.AT_SYMLINK_NOFOLLOW)
}
