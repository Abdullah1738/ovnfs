//go:build darwin

package repo

import (
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// realPath returns the physical path of an existing file as git's
// realpath(3) reports it on macOS: symlinks (including /tmp → /private/tmp)
// resolved and each component in its on-disk case. It asks the kernel for
// the open file's path (F_GETPATH), falling back to resolving symlinks.
func realPath(p string) (string, error) {
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	if resolved, err = filepath.Abs(resolved); err != nil {
		return "", err
	}
	fd, err := unix.Open(resolved, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return resolved, nil
	}
	defer unix.Close(fd)
	var buf [unix.PathMax]byte
	_, err = unix.FcntlInt(uintptr(fd), unix.F_GETPATH, int(uintptr(unsafe.Pointer(&buf[0]))))
	runtime.KeepAlive(&buf)
	if err != nil {
		return resolved, nil
	}
	n := 0
	for n < len(buf) && buf[n] != 0 {
		n++
	}
	return string(buf[:n]), nil
}
