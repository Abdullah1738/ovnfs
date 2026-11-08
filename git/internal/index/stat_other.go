//go:build !(linux || darwin || freebsd || netbsd || openbsd)

package index

// platformStat reports that ctime, inode, uid and gid are unavailable here
// (Windows, among others), so racy checks compare size and mtime only.
func platformStat(string) (Stat, bool) { return Stat{}, false }
