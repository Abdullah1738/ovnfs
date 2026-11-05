//go:build darwin || linux

package overlay

import (
	"os"
	"syscall"
)

// umask is the process's file creation mask, applied to memory files the way
// the kernel applies it to host creates.
var umask = func() os.FileMode {
	old := syscall.Umask(0)
	syscall.Umask(old)
	return os.FileMode(old) & os.ModePerm
}()

// dirStat returns a directory's host stat data, if it has any.
func dirStat(dir os.FileInfo) (*syscall.Stat_t, bool) {
	if dir == nil {
		return nil, false
	}
	st, ok := dir.Sys().(*syscall.Stat_t)
	return st, ok
}

// matchOwner gives the new file f the owner a memory file showed, when its
// staging directory handed out another group.
func matchOwner(f *os.File, uid, gid uint32) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && (st.Uid != uid || st.Gid != gid) {
		return f.Chown(int(uid), int(gid))
	}
	return nil
}

// unixMode encodes a memory file's mode as stat st_mode bits.
func unixMode(mode os.FileMode) uint32 {
	m := syscall.S_IFREG | uint32(mode.Perm())
	if mode&os.ModeSetuid != 0 {
		m |= syscall.S_ISUID
	}
	if mode&os.ModeSetgid != 0 {
		m |= syscall.S_ISGID
	}
	if mode&os.ModeSticky != 0 {
		m |= syscall.S_ISVTX
	}
	return m
}
