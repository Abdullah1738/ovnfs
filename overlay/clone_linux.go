//go:build linux

package overlay

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// cloneTree reflinks regular files on supported Linux filesystems, copying otherwise.
func cloneTree(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return copyTree(src, dst)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	err = unix.IoctlFileClone(int(out.Fd()), int(in.Fd()))
	if err != nil {
		out.Close()
		if removeErr := os.Remove(dst); removeErr != nil {
			return removeErr
		}
		if errors.Is(err, unix.EXDEV) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.ENOSYS) {
			return copyTree(src, dst)
		}
		return err
	}
	if err := out.Chmod(info.Mode().Perm()); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chtimes(dst, info.ModTime(), info.ModTime())
}
