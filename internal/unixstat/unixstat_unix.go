//go:build unix

package unixstat

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// native reads a syscall.Stat_t, which host files and memory files carry.
func native(sys any) (Info, bool) {
	st, ok := sys.(*syscall.Stat_t)
	if !ok || st == nil {
		return Info{}, false
	}
	rdev := uint64(st.Rdev)
	return Info{
		Nlink: uint32(st.Nlink),
		UID:   st.Uid,
		GID:   st.Gid,
		Major: unix.Major(rdev),
		Minor: unix.Minor(rdev),
	}, true
}
