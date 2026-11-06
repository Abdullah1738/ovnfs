package overlay

import (
	"os"
	"syscall"
	"time"
)

// memStat builds linux stat data for a memory file.
func memStat(i memInfo) any {
	s := &syscall.Stat_t{Nlink: 1, Uid: i.uid, Gid: i.gid, Ino: i.ino, Size: i.size, Blocks: (i.size + 511) / 512, Blksize: 4096}
	s.Mode = unixMode(i.mode)
	s.Atim = syscall.NsecToTimespec(i.atime.UnixNano())
	s.Mtim = syscall.NsecToTimespec(i.mtime.UnixNano())
	s.Ctim = syscall.NsecToTimespec(i.ctime.UnixNano())
	return s
}

// setCreationTime leaves the Linux creation time to the filesystem.
func setCreationTime(string, time.Time) error { return nil }

// birthTime reports no creation time: Linux leaves it to the filesystem.
func birthTime(os.FileInfo) (time.Time, bool) { return time.Time{}, false }

// newOwner returns the owner a file created now in directory dir gets: the
// effective uid, and the effective gid unless dir is setgid, whose group it
// then inherits.
func newOwner(dir os.FileInfo) (uid, gid uint32) {
	uid, gid = uint32(os.Geteuid()), uint32(os.Getegid())
	if st, ok := dirStat(dir); ok && st.Mode&syscall.S_ISGID != 0 {
		gid = st.Gid
	}
	return uid, gid
}
