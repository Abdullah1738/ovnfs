//go:build darwin || linux

package base

import (
	"os"
	"syscall"
	"time"
)

// unixType maps a file mode's type to the stat S_IF* bits.
func unixType(mode os.FileMode) uint32 {
	switch {
	case mode.IsDir():
		return syscall.S_IFDIR
	case mode&os.ModeSymlink != 0:
		return syscall.S_IFLNK
	default:
		return syscall.S_IFREG
	}
}

// syntheticSys builds stat data for a file that exists only as offered metadata,
// owned by this process's user so frontends do not report root ownership.
func syntheticSys(mode os.FileMode, size int64, mtime time.Time) any {
	s := &syscall.Stat_t{Nlink: 1, Uid: uint32(os.Getuid()), Gid: uint32(os.Getgid()), Size: size}
	setStatMode(s, unixType(mode)|uint32(mode.Perm()))
	setStatMtime(s, syscall.NsecToTimespec(mtime.UnixNano()))
	return s
}
