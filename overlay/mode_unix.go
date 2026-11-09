//go:build !windows

package overlay

import (
	"io/fs"
	"net"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Unix files hold their mode natively; see mode_windows.go for hosts that cannot.

// sameFile is [os.SameFile] for metadata that may carry an alias name.
func sameFile(a, b os.FileInfo) bool { return os.SameFile(unalias(a), unalias(b)) }

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

// makeNode makes only data-free nodes. A short symlink path permits Unix
// socket creation even when the upper's full path exceeds sockaddr_un's limit.
func makeNode(name string, mode os.FileMode) error {
	switch mode & os.ModeType {
	case os.ModeNamedPipe:
		if err := unix.Mkfifo(name, uint32(mode.Perm())); err != nil {
			return &fs.PathError{Op: "mknod", Path: name, Err: err}
		}
	case os.ModeSocket:
		// Bind under a short alias, then link the inode into place. This also
		// handles a long final name without overwriting an existing entry.
		parentTmp, err := os.MkdirTemp(filepath.Dir(name), ".ovnfs-node-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(parentTmp)
		tmp, err := os.MkdirTemp("", "ovnfs-node-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		link := filepath.Join(tmp, "d")
		if err := os.Symlink(parentTmp, link); err != nil {
			return err
		}
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(link, "s"), Net: "unix"})
		if err != nil {
			return err
		}
		listener.SetUnlinkOnClose(false)
		if err := listener.Close(); err != nil {
			return err
		}
		node := filepath.Join(parentTmp, "s")
		if err := os.Chmod(node, mode.Perm()); err != nil {
			return err
		}
		if err := os.Link(node, name); err != nil {
			return err
		}

	default:
		return &fs.PathError{Op: "mknod", Path: name, Err: unix.EPERM}
	}
	return nil
}
