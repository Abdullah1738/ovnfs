package source

import (
	"os"
	"syscall"
)

// statKey reads the inode and inode-change time from stat data.
func statKey(info os.FileInfo) fileKey {
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileKey{}
	}
	return fileKey{ino: s.Ino, ctime: s.Ctimespec.Nano()}
}
