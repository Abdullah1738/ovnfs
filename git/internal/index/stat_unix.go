//go:build linux || darwin || freebsd || netbsd || openbsd

package index

import "golang.org/x/sys/unix"

// platformStat returns the stat fields fs.FileInfo does not expose for name:
// ctime, inode, uid and gid. Only those fields of the result are set.
func platformStat(name string) (Stat, bool) {
	var st unix.Stat_t
	if err := unix.Lstat(name, &st); err != nil {
		return Stat{}, false
	}
	return Stat{
		CTime: Time{Sec: uint32(st.Ctim.Sec), Nsec: uint32(st.Ctim.Nsec)},
		Ino:   uint32(st.Ino),
		UID:   st.Uid,
		GID:   st.Gid,
	}, true
}
