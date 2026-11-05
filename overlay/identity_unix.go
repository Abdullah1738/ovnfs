//go:build !windows

package overlay

import (
	"io/fs"
	"os"
	"syscall"
)

// primeInfo is unnecessary on Unix because Lstat already contains inode identity.
func primeInfo(os.FileInfo) error { return nil }

// identity extracts stable host inode identity from existing Unix stat metadata.
func identity(_ string, p string, info os.FileInfo) (inodeKey, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return inodeKey{}, &fs.PathError{Op: "inode", Path: p, Err: fs.ErrInvalid}
	}
	return inodeKey{device: uint64(st.Dev), inode: uint64(st.Ino)}, nil
}
