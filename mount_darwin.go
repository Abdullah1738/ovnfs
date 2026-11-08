//go:build darwin

package main

import "golang.org/x/sys/unix"

// mounted reads cached mount records without issuing I/O to a mounted
// filesystem; it errs toward true when they cannot be read.
func mounted(dir string) bool {
	_, found, err := mountRecord(dir)
	return found || err != nil
}

// mountAtime reports whether the mount at dir updates access times; ok is
// false when dir is not a mount point or the records cannot be read.
func mountAtime(dir string) (atime, ok bool) {
	entry, found, err := mountRecord(dir)
	if err != nil || !found {
		return false, false
	}
	return entry.Flags&unix.MNT_NOATIME == 0, true
}

// remountCmd updates the mount at dir with generic option opt. An update
// replaces every generic flag, so it restates mountCmd's nobrowse.
func remountCmd(dir, opt string) []string {
	return []string{"mount", "-u", "-o", "nobrowse," + opt, dir}
}

// mountRecord finds dir's cached mount record.
func mountRecord(dir string) (unix.Statfs_t, bool, error) {
	target := mountPath(dir)
	count, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil {
		return unix.Statfs_t{}, false, err
	}
	entries := make([]unix.Statfs_t, count+16)
	count, err = unix.Getfsstat(entries, unix.MNT_NOWAIT)
	if err != nil {
		return unix.Statfs_t{}, false, err
	}
	for _, entry := range entries[:count] {
		if unix.ByteSliceToString(entry.Mntonname[:]) == target {
			return entry, true, nil
		}
	}
	return unix.Statfs_t{}, false, nil
}
