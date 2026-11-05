//go:build darwin

package main

import "golang.org/x/sys/unix"

// mounted reads cached mount records without issuing I/O to a mounted filesystem.
func mounted(dir string) bool {
	target := mountPath(dir)
	count, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil {
		return true
	}
	entries := make([]unix.Statfs_t, count+16)
	count, err = unix.Getfsstat(entries, unix.MNT_NOWAIT)
	if err != nil {
		return true
	}
	for _, entry := range entries[:count] {
		if unix.ByteSliceToString(entry.Mntonname[:]) == target {
			return true
		}
	}
	return false
}
