//go:build unix

package repo

import (
	"os"
	"strconv"
	"syscall"
)

// device returns the ID of the filesystem holding path.
func device(path string) (uint64, bool) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, false
	}
	return uint64(st.Dev), true
}

// ownedByCurrentUser reports whether path's owner is the effective user or,
// when running as root, the user named by SUDO_UID (config/safe.adoc).
func ownedByCurrentUser(path string) (bool, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return false, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	euid := os.Geteuid()
	if uint64(st.Uid) == uint64(euid) {
		return true, nil
	}
	if euid == 0 {
		if v, ok := os.LookupEnv("SUDO_UID"); ok {
			if uid, err := strconv.ParseUint(v, 10, 32); err == nil && uid == uint64(st.Uid) {
				return true, nil
			}
		}
	}
	return false, nil
}
