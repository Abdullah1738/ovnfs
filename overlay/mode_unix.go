//go:build !windows

package overlay

import (
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

// Unix files hold their mode natively; see mode_windows.go for hosts that cannot.

// sameFile is [os.SameFile].
func sameFile(a, b os.FileInfo) bool { return os.SameFile(a, b) }

// hostLstat is [os.Lstat].
func hostLstat(name string) (os.FileInfo, error) { return os.Lstat(name) }

// lstatIndexed is [os.Lstat]: the host's mode is current.
func lstatIndexed(name string, _ os.FileInfo) (os.FileInfo, error) { return os.Lstat(name) }

// withMode returns info: the host's mode is the file's.
func withMode(_ string, info os.FileInfo) (os.FileInfo, error) { return info, nil }

// keepMode has nothing to add after the host chmod or create that set the mode.
func keepMode(string, os.FileMode) error { return nil }

// lchmod changes the symlink name's own mode; Linux refuses it with EOPNOTSUPP.
func lchmod(name string, mode os.FileMode) error {
	bits := uint32(mode.Perm())
	if mode&os.ModeSetuid != 0 {
		bits |= unix.S_ISUID
	}
	if mode&os.ModeSetgid != 0 {
		bits |= unix.S_ISGID
	}
	if mode&os.ModeSticky != 0 {
		bits |= unix.S_ISVTX
	}
	if err := unix.Fchmodat(unix.AT_FDCWD, name, bits, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return &fs.PathError{Op: "chmod", Path: name, Err: err}
	}
	return nil
}
