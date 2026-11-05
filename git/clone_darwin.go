//go:build darwin

package git

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// cloneIndexFile shares APFS data blocks for a new index snapshot, leaving
// unsupported filesystems and existing destinations to the streaming copy path.
func cloneIndexFile(src, dst string) (bool, error) {
	err := unix.Clonefile(src, dst, 0)
	if err == nil {
		if err := os.Chmod(dst, 0600); err != nil {
			os.Remove(dst)
			return false, err
		}
		return true, nil
	}
	if errors.Is(err, unix.EXDEV) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EEXIST) {
		return false, nil
	}
	return false, err
}
